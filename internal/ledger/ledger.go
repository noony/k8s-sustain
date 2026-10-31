// Package ledger is the controller's memory of applying each live member,
// across passes and Policies, and the one place its verdicts are taken:
// whether a member may run this pass, whether it is Blocked and on which
// step, the Warning a result calls for, the health of each identity over its
// members, and when a retry calls for the next pass. Results go in, verdicts
// come out; nothing else decides them.
package ledger

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// A transient failure backs off BaseRetryDelay, doubling with each
// consecutive transient failure, up to MaxRetryDelay.
const (
	BaseRetryDelay = 30 * time.Second
	MaxRetryDelay  = 5 * time.Minute
)

var stepFailures = map[workload.Step]string{
	workload.StepPrometheus: "Prometheus query failed",
	workload.StepPatch:      "Pod recycle failed",
	workload.StepResize:     "In-place pod resize failed",
	workload.StepStore:      "Recommendation could not be stored for the webhook",
}

// MemberKey names a member across passes and Policies.
type MemberKey struct {
	Kind, Namespace, Name string
}

func (k MemberKey) String() string {
	return k.Kind + "/" + k.Namespace + "/" + k.Name
}

// Kind is what one pass did with one live member.
type Kind int

const (
	// Done: the member's pods were driven toward the Recommendation, or
	// counted under OnCreate.
	Done Kind = iota
	// Idle: there was nothing to apply, no Recommendation for the member's
	// containers or recommend-only. Its pod counts are dropped.
	Idle
	// Uncounted: an OnCreate count failed. Not a failure: the pass touches
	// no pod, and backoff would leave the identity unfetched and freeze the
	// WorkloadRecommendation the webhook reads. The member keeps its last pod
	// counts.
	Uncounted
	// Skipped: the member was left alone, in retry backoff or at shutdown.
	// It keeps its state.
	Skipped
	// Failed: a step failed, applying or storing what the pods would read;
	// the ledger classifies Err.
	Failed
)

// Result is what one pass did with one live member.
type Result struct {
	Member MemberKey
	Kind   Kind
	// Pods is the member's pod counts, set for Done.
	Pods workload.PodCounts
	// Step and Err are the step that failed and why, set for Failed.
	Step workload.Step
	Err  error
}

// Event is the Warning a member's result calls for.
type Event struct {
	Reason, Message string
}

// Health is an identity's, over the members every Policy claims for it.
type Health struct {
	// Pods sums the counts of the last measured pass of each member that had
	// one, so a member skipped or failed since contributes its last counts;
	// Measured is false when none had.
	Pods     workload.PodCounts
	Measured bool
	// BlockedOn is the step its first Blocked member, in key order, keeps
	// failing on, empty unless one is Blocked.
	BlockedOn workload.Step
	// Attempts is how many of its members failed in the pass settled.
	Attempts int
}

// Blocked reports whether one of its members is Blocked.
func (h Health) Blocked() bool { return h.BlockedOn != "" }

// Failed reports whether one of its members failed in the pass settled.
func (h Health) Failed() bool { return h.Attempts > 0 }

// Verdict is the ledger's answer to one pass of a Policy.
type Verdict struct {
	// Identities is the health of every identity the pass reported.
	Identities map[promclient.WorkloadIdentity]Health
	// Events is the Warning each member that calls for one gets, once per
	// transition: every transient attempt, naming when the next runs, and a
	// permanent failure only when the member enters it.
	Events map[MemberKey]Event
}

// Blocked counts the identities with a Blocked member.
func (v Verdict) Blocked() int {
	n := 0
	for _, h := range v.Identities {
		if h.Blocked() {
			n++
		}
	}
	return n
}

// Ledger holds, per member, whether it is Blocked and on which step, its last
// measured pod counts and when its retry is due, and, per Policy, the members
// its last pass reported.
//
// An identity is governed by at most one Policy at a time; its members are
// still unioned across Policies, so an identity moving between two keeps one
// health while both claim it, and a member is forgotten once no Policy claims
// it.
type Ledger struct {
	clock clock.PassiveClock

	mu      sync.Mutex
	members map[MemberKey]*memberEntry
	claims  map[string]map[promclient.WorkloadIdentity][]MemberKey
}

type memberEntry struct {
	pods     workload.PodCounts
	measured bool
	failure  *failure
}

// failure is a member's run of consecutive failed passes.
type failure struct {
	step      workload.Step
	permanent bool
	// attempts counts the consecutive transient failures the backoff grows
	// with, zero for a permanent one: a permanent failure in between restarts
	// it.
	attempts int
	// due is when a transient failure may run again. A permanent one runs
	// every pass, so a fixed cause (a missing RBAC rule) is picked up without
	// a restart.
	due time.Time
}

// New is an empty ledger whose backoff runs on clk.
func New(clk clock.PassiveClock) *Ledger {
	return &Ledger{
		clock:   clk,
		members: map[MemberKey]*memberEntry{},
		claims:  map[string]map[promclient.WorkloadIdentity][]MemberKey{},
	}
}

// MayRun reports whether member may be applied this pass: false while its
// transient failure backs off.
func (l *Ledger) MayRun(member MemberKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.members[member]
	return e == nil || e.failure == nil || e.failure.permanent || !l.clock.Now().Before(e.failure.due)
}

// Settle takes one pass of policy: per live identity it governs, one result
// per live member, skipped ones included. It records every result, makes the
// pass's members the ones policy claims, forgets those no Policy claims any
// more, and returns the health of every identity of the pass and the Warnings
// its results call for.
func (l *Ledger) Settle(policy string, pass map[promclient.WorkloadIdentity][]Result) Verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := Verdict{
		Identities: make(map[promclient.WorkloadIdentity]Health, len(pass)),
		Events:     map[MemberKey]Event{},
	}
	claims := make(map[promclient.WorkloadIdentity][]MemberKey, len(pass))
	attempts := make(map[promclient.WorkloadIdentity]int)
	for id, results := range pass {
		keys := make([]MemberKey, len(results))
		for i, res := range results {
			keys[i] = res.Member
			ev, failed := l.recordLocked(res)
			if !failed {
				continue
			}
			attempts[id]++
			if ev != nil {
				v.Events[res.Member] = *ev
			}
		}
		claims[id] = keys
	}
	l.claimLocked(policy, claims)
	for id := range pass {
		h := l.healthLocked(id)
		h.Attempts = attempts[id]
		v.Identities[id] = h
	}
	return v
}

// Forget drops what policy claims, as for a deleted Policy, and forgets the
// members no other Policy claims.
func (l *Ledger) Forget(policy string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claimLocked(policy, nil)
}

// RequeueAfter is how long until policy's next pass: interval, or the time
// until the earliest retry among the members it claims falls due when that
// comes first, so the time a retry event names is a time a pass runs. A retry
// already due brings it no lower than a second: a zero RequeueAfter means no
// requeue at all.
func (l *Ledger) RequeueAfter(policy string, interval time.Duration) time.Duration {
	return l.requeueAfter(policy, interval, time.Second)
}

// RequeueAfterListFailure is RequeueAfter for a pass of policy that could not
// list its workloads, which leaves policy's claims as its last pass left
// them. A retry already due cannot run until a pass lists again, so it would
// otherwise requeue at once, pass after pass: it brings the requeue no lower
// than BaseRetryDelay.
func (l *Ledger) RequeueAfterListFailure(policy string, interval time.Duration) time.Duration {
	return l.requeueAfter(policy, interval, BaseRetryDelay)
}

func (l *Ledger) requeueAfter(policy string, interval, floor time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var earliest time.Time
	for _, members := range l.claims[policy] {
		for _, m := range members {
			e := l.members[m]
			if e == nil || e.failure == nil || e.failure.permanent {
				continue
			}
			if earliest.IsZero() || e.failure.due.Before(earliest) {
				earliest = e.failure.due
			}
		}
	}
	if earliest.IsZero() {
		return interval
	}
	return min(interval, max(earliest.Sub(l.clock.Now()), floor))
}

// recordLocked takes one pass's result for a member. failed reports whether
// it counts as a failure of the pass, and ev is the Warning it calls for.
func (l *Ledger) recordLocked(res Result) (ev *Event, failed bool) {
	switch res.Kind {
	case Skipped:
		return nil, false
	case Failed:
		return l.failLocked(res), true
	}
	e := l.entryLocked(res.Member)
	e.failure = nil
	switch res.Kind {
	case Done:
		e.pods, e.measured = res.Pods, true
	case Idle:
		e.measured = false
	}
	if !e.measured {
		delete(l.members, res.Member)
	}
	return nil, false
}

func (l *Ledger) entryLocked(key MemberKey) *memberEntry {
	e := l.members[key]
	if e == nil {
		e = &memberEntry{}
		l.members[key] = e
	}
	return e
}

func (l *Ledger) failLocked(res Result) *Event {
	e := l.entryLocked(res.Member)
	prev := e.failure
	f := &failure{step: res.Step, permanent: permanentError(res.Err)}
	e.failure = f
	what := stepFailures[res.Step]
	if f.permanent {
		if prev != nil && prev.permanent {
			return nil
		}
		return &Event{
			Reason:  "ReconciliationFailed",
			Message: fmt.Sprintf("%s: %v. Permanent error, retried every reconcile without backoff", what, res.Err),
		}
	}
	f.attempts = 1
	if prev != nil {
		f.attempts += prev.attempts
	}
	// time.Duration is an int64 of nanoseconds, so `BaseRetryDelay << shift`
	// overflows for a large shift and wraps to zero once shift >= 64.
	// maxShift=16 puts 30s << 16 ≈ 23 days: far above MaxRetryDelay, nowhere
	// near overflow.
	const maxShift = 16
	f.due = l.clock.Now().Add(min(BaseRetryDelay<<min(f.attempts-1, maxShift), MaxRetryDelay))
	return &Event{
		Reason:  "ReconciliationRetryScheduled",
		Message: fmt.Sprintf("%s: %v. Attempt %d failed, retrying at %s", what, res.Err, f.attempts, f.due.Format(time.RFC3339)),
	}
}

// permanentError reports whether retrying err cannot help until something
// outside the controller changes: a member workload.Apply refuses, or an API
// rejection (403, 401, Invalid, …) anywhere in its tree, so one forbidden
// eviction among several pod errors is enough. Everything else (Prometheus
// errors and timeouts, 5xx, network errors) is transient, and so are a 429,
// a 409, an optimistic-concurrency loss the next attempt re-reads past, and a
// 404, an object deleted since it was read: a WorkloadRecommendation deleted
// between its claim and its record is re-created by the next claim.
func permanentError(err error) bool {
	return errors.Is(err, workload.ErrRefused) || rejected(err)
}

func rejected(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := err.(apierrors.APIStatus); ok {
		code := s.Status().Code
		if code >= 400 && code < 500 && code != http.StatusNotFound && code != http.StatusConflict && code != http.StatusTooManyRequests {
			return true
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if rejected(e) {
				return true
			}
		}
		return false
	}
	return rejected(errors.Unwrap(err))
}
