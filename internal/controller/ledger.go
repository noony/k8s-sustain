package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"

	"github.com/noony/k8s-sustain/internal/workload"
)

const (
	baseRetryDelay = 30 * time.Second
	maxRetryDelay  = 5 * time.Minute
)

// The step a member failed on: the reason its Blocked identity reports.
const (
	phasePrometheus = "prometheus"
	phasePatch      = "patch"
	phaseResize     = "resize"
	phaseStore      = "store"
)

var stepFailures = map[string]string{
	phasePrometheus: "Prometheus query failed",
	phasePatch:      "Pod recycle failed",
	phaseResize:     "In-place pod resize failed",
	phaseStore:      "Recommendation could not be stored for the webhook",
}

// applyKind is what one pass did with one live member.
type applyKind int

const (
	// applyDone: the member's pods were driven toward the Recommendation, or
	// counted under OnCreate.
	applyDone applyKind = iota
	// applyIdle: there was nothing to apply, no Recommendation for the
	// member's containers or recommend-only. Its pod counts are dropped.
	applyIdle
	// applyUncounted: an OnCreate count failed. Not a failure: the pass
	// touches no pod, and backoff would leave the identity unfetched and
	// freeze the WorkloadRecommendation the webhook reads. The member keeps
	// its last pod counts.
	applyUncounted
	// applySkipped: the member was left alone, in retry backoff or at
	// shutdown. It keeps its state.
	applySkipped
	// applyFailed: a step failed, applying or storing what the pods would
	// read; the ledger classifies err.
	applyFailed
)

// applyResult is what one pass did with one live member, the apply step's
// only output and the ledger's only input.
type applyResult struct {
	member *workloadTarget
	kind   applyKind
	// pods is the member's pod counts, set for applyDone.
	pods workload.PodCounts
	// phase and err are the step that failed and why, set for applyFailed.
	phase string
	err   error
}

// ledgerEvent is the Warning a recorded failure calls for on the member.
type ledgerEvent struct {
	reason  string
	message string
}

// memberHealth is what the ledger holds on one member for its identity's
// health series.
type memberHealth struct {
	// pods is the member's counts from its last measured pass; measured is
	// false when there are none.
	pods     workload.PodCounts
	measured bool
	// blockedOn is the step the member keeps failing on, empty unless it is
	// Blocked.
	blockedOn string
}

// ledger is the controller's memory of applying each live member, across
// passes and Policies: whether it may run this pass, whether it is Blocked
// and on which step, its last measured pod counts, and when its retry is
// due. Results go in, verdicts come out; nothing else decides them. Keyed
// by workloadTarget.key().
type ledger struct {
	clock clock.PassiveClock

	mu      sync.Mutex
	members map[string]*memberEntry
}

type memberEntry struct {
	pods     workload.PodCounts
	measured bool
	failure  *failure
}

// failure is a member's run of consecutive failed passes.
type failure struct {
	phase     string
	permanent bool
	attempts  int
	// due is when a transient failure may run again. A permanent one runs
	// every pass, so a fixed cause (a missing RBAC rule) is picked up without
	// a restart.
	due time.Time
}

func newLedger(clk clock.PassiveClock) *ledger {
	return &ledger{clock: clk, members: map[string]*memberEntry{}}
}

// mayRun reports whether member may be applied this pass: false while its
// transient failure backs off.
func (l *ledger) mayRun(member string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.members[member]
	return e == nil || e.failure == nil || e.failure.permanent || !l.clock.Now().Before(e.failure.due)
}

// record takes one pass's result for a member. failed reports whether it
// counts as a failure of the pass; ev is the Warning to emit, once per
// transition: every transient attempt, naming when the next runs, and a
// permanent failure only when the member enters it.
func (l *ledger) record(ctx context.Context, res applyResult) (ev *ledgerEvent, failed bool) {
	key := res.member.key()
	l.mu.Lock()
	defer l.mu.Unlock()
	switch res.kind {
	case applySkipped:
		return nil, false
	case applyFailed:
		// Only the reconcile's own context ending is shutdown: an error that
		// merely wraps a context error comes from a call that ran out of its
		// own timeout.
		if ctx.Err() != nil {
			return nil, false
		}
		return l.failLocked(key, res), true
	}
	e := l.entryLocked(key)
	e.failure = nil
	switch res.kind {
	case applyDone:
		e.pods, e.measured = res.pods, true
	case applyIdle:
		e.measured = false
	}
	if !e.measured {
		delete(l.members, key)
	}
	return nil, false
}

func (l *ledger) entryLocked(key string) *memberEntry {
	e := l.members[key]
	if e == nil {
		e = &memberEntry{}
		l.members[key] = e
	}
	return e
}

func (l *ledger) failLocked(key string, res applyResult) *ledgerEvent {
	e := l.entryLocked(key)
	prev := e.failure
	f := &failure{phase: res.phase, permanent: permanentError(res.err), attempts: 1}
	if prev != nil {
		f.attempts = prev.attempts + 1
	}
	e.failure = f
	what := stepFailures[res.phase]
	if f.permanent {
		if prev != nil && prev.permanent {
			return nil
		}
		return &ledgerEvent{
			reason:  "ReconciliationFailed",
			message: fmt.Sprintf("%s: %v. Permanent error, retried every reconcile without backoff", what, res.err),
		}
	}
	// time.Duration is an int64 of nanoseconds, so `baseRetryDelay << shift`
	// overflows for a large shift and wraps to zero once shift >= 64.
	// maxShift=16 puts 30s << 16 ≈ 23 days: far above maxRetryDelay, nowhere
	// near overflow.
	const maxShift = 16
	f.due = l.clock.Now().Add(min(baseRetryDelay<<min(f.attempts-1, maxShift), maxRetryDelay))
	return &ledgerEvent{
		reason:  "ReconciliationRetryScheduled",
		message: fmt.Sprintf("%s: %v. Attempt %d failed, retrying at %s", what, res.err, f.attempts, f.due.Format(time.RFC3339)),
	}
}

// health is what the ledger holds on member for its identity's series.
func (l *ledger) health(member string) memberHealth {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.members[member]
	if e == nil {
		return memberHealth{}
	}
	h := memberHealth{pods: e.pods, measured: e.measured}
	if e.failure != nil {
		h.blockedOn = e.failure.phase
	}
	return h
}

// nextRetry is how long until the earliest transient retry among members
// falls due, possibly already past; false when none of them backs off.
func (l *ledger) nextRetry(members []string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var earliest time.Time
	for _, m := range members {
		e := l.members[m]
		if e == nil || e.failure == nil || e.failure.permanent {
			continue
		}
		if earliest.IsZero() || e.failure.due.Before(earliest) {
			earliest = e.failure.due
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	return earliest.Sub(l.clock.Now()), true
}

// forget drops member, once no Policy claims it.
func (l *ledger) forget(member string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.members, member)
}

// permanentError reports whether retrying err cannot help until something
// outside the controller changes: a member workload.Apply refuses, or an API
// rejection (403, 401, Invalid, …) anywhere in its tree, so one forbidden
// eviction among several pod errors is enough. Everything else (Prometheus
// errors and timeouts, 5xx, network errors) is transient, and so are a 429
// and a 409, an optimistic-concurrency loss the next attempt re-reads past.
func permanentError(err error) bool {
	return errors.Is(err, workload.ErrRefused) || rejected(err)
}

func rejected(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := err.(apierrors.APIStatus); ok {
		code := s.Status().Code
		if code >= 400 && code < 500 && code != http.StatusConflict && code != http.StatusTooManyRequests {
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
