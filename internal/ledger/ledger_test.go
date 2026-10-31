package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/noony/k8s-sustain/internal/ledger"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

var (
	unavailable = apierrors.NewServiceUnavailable("apiserver unavailable")
	forbidden   = apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"}, "web-1", errors.New("no RBAC"))
)

var (
	web = promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}
	api = promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "api"}
)

func onFakeClock() (*ledger.Ledger, *clocktesting.FakePassiveClock) {
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	return ledger.New(clk), clk
}

func deployment(name string) ledger.MemberKey {
	return ledger.MemberKey{Kind: "Deployment", Namespace: "default", Name: name}
}

func failedOn(m ledger.MemberKey, step workload.Step, err error) ledger.Result {
	return ledger.Result{Member: m, Kind: ledger.Failed, Step: step, Err: err}
}

func resultOf(m ledger.MemberKey, kind ledger.Kind) ledger.Result {
	return ledger.Result{Member: m, Kind: kind}
}

func measured(m ledger.MemberKey, total, stale int) ledger.Result {
	return ledger.Result{Member: m, Kind: ledger.Done, Pods: workload.PodCounts{Total: total, Stale: stale}}
}

// settle is a pass of Policy p over identity id, whose members got results.
func settle(l *ledger.Ledger, id promclient.WorkloadIdentity, results ...ledger.Result) ledger.Verdict {
	return l.Settle("p", map[promclient.WorkloadIdentity][]ledger.Result{id: results})
}

func TestSettle_Result(t *testing.T) {
	member := deployment("web")
	failed := func(step workload.Step, err error) ledger.Result { return failedOn(member, step, err) }
	succeeded := func(kind ledger.Kind) ledger.Result { return resultOf(member, kind) }
	cases := []struct {
		name        string
		before      []ledger.Result
		res         ledger.Result
		wantFailed  bool
		wantEvent   string
		wantBlocked workload.Step
		wantMayRun  bool
	}{
		{
			name: "transient failure backs off", res: failed(workload.StepPatch, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepPatch,
		},
		{
			name: "query timeout is a transient failure", res: failed(workload.StepPrometheus, fmt.Errorf("querying cpu: %w", context.DeadlineExceeded)),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepPrometheus,
		},
		{
			name: "every transient attempt is warned about", before: []ledger.Result{failed(workload.StepPatch, unavailable)}, res: failed(workload.StepPatch, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepPatch,
		},
		{
			name: "permanent failure blocks without backoff", res: failed(workload.StepPatch, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: workload.StepPatch, wantMayRun: true,
		},
		{
			name: "permanent failure is warned about once", before: []ledger.Result{failed(workload.StepPatch, forbidden)}, res: failed(workload.StepPatch, forbidden),
			wantFailed: true, wantBlocked: workload.StepPatch, wantMayRun: true,
		},
		{
			name: "transient failure turning permanent is warned about", before: []ledger.Result{failed(workload.StepPatch, unavailable)}, res: failed(workload.StepPatch, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: workload.StepPatch, wantMayRun: true,
		},
		{
			name: "a conflict is retried with backoff", res: failed(workload.StepResize, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "web-1", errors.New("modified"))),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepResize,
		},
		{
			name: "a refused member blocks without backoff", res: failed(workload.StepPatch, fmt.Errorf("%w: Deployment default/web has no selector", workload.ErrRefused)),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: workload.StepPatch, wantMayRun: true,
		},
		{
			name: "member withheld for a failed write counts", res: failed(workload.StepStore, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepStore,
		},
		{
			name: "member withheld for a record whose object was deleted since its claim backs off", res: failed(workload.StepStore,
				apierrors.NewNotFound(schema.GroupResource{Resource: "workloadrecommendations"}, "deployment-web")),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: workload.StepStore,
		},
		{
			name: "member withheld for a forbidden Ensure counts", res: failed(workload.StepStore, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: workload.StepStore, wantMayRun: true,
		},
		{
			name: "applying clears a failure", before: []ledger.Result{failed(workload.StepPatch, forbidden)}, res: succeeded(ledger.Done),
			wantMayRun: true,
		},
		{
			name: "nothing to apply clears a failure", before: []ledger.Result{failed(workload.StepPatch, unavailable)}, res: succeeded(ledger.Idle),
			wantMayRun: true,
		},
		{
			name: "a failed OnCreate count clears a failure", before: []ledger.Result{failed(workload.StepPatch, unavailable)}, res: succeeded(ledger.Uncounted),
			wantMayRun: true,
		},
		{
			name: "a skipped member stays Blocked", before: []ledger.Result{failed(workload.StepPatch, unavailable)}, res: succeeded(ledger.Skipped),
			wantBlocked: workload.StepPatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := onFakeClock()
			for _, res := range tc.before {
				settle(l, web, res)
			}

			v := settle(l, web, tc.res)

			h := v.Identities[web]
			if h.Failed() != tc.wantFailed {
				t.Errorf("failed = %v, want %v", h.Failed(), tc.wantFailed)
			}
			if got := v.Events[member].Reason; got != tc.wantEvent {
				t.Errorf("event = %q, want %q", got, tc.wantEvent)
			}
			if h.BlockedOn != tc.wantBlocked || h.Blocked() != (tc.wantBlocked != "") {
				t.Errorf("blocked on %q (Blocked %v), want %q", h.BlockedOn, h.Blocked(), tc.wantBlocked)
			}
			if got := l.MayRun(member); got != tc.wantMayRun {
				t.Errorf("MayRun = %v, want %v", got, tc.wantMayRun)
			}
		})
	}
}

// A refusal or an API rejection anywhere in the error tree is permanent: the
// member is retried every pass and warned about with ReconciliationFailed.
// Everything else backs off.
func TestSettle_ClassifiesTheError(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{"prometheus error", errors.New("prometheus query failed"), false},
		{"query timeout", fmt.Errorf("querying cpu: %w", context.DeadlineExceeded), false},
		{"server timeout", apierrors.NewServerTimeout(pods, "list", 5), false},
		{"throttled", apierrors.NewTooManyRequests("slow down", 5), false},
		{"conflict", apierrors.NewConflict(pods, "web-1", errors.New("the object has been modified")), false},
		{"wrapped conflict", fmt.Errorf("pod web-1: %w", apierrors.NewConflict(pods, "web-1", errors.New("modified"))), false},
		{"apply refusal", fmt.Errorf("%w: Deployment default/web has no UID", workload.ErrRefused), true},
		{"apply refusal among pod errors", errors.Join(fmt.Errorf("pod web-1: %w", unavailable), fmt.Errorf("%w: no selector", workload.ErrRefused)), true},
		{"unavailable", unavailable, false},
		{"internal error", apierrors.NewInternalError(errors.New("oops")), false},
		{"forbidden", forbidden, true},
		{"unauthorized", apierrors.NewUnauthorized("expired token"), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "web", nil), true},
		{"not found", apierrors.NewNotFound(pods, "web"), false},
		{"wrapped not found", fmt.Errorf("recording: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "workloadrecommendations"}, "web")), false},
		{"wrapped forbidden", fmt.Errorf("pod web-1: %w", forbidden), true},
		{"one forbidden pod among transient ones", errors.Join(
			fmt.Errorf("pod web-1: %w", unavailable), fmt.Errorf("pod web-2: %w", forbidden)), true},
		{"only transient pods", errors.Join(
			fmt.Errorf("pod web-1: %w", unavailable), fmt.Errorf("pod web-2: %w", unavailable)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := onFakeClock()
			member := deployment("web")

			v := settle(l, web, failedOn(member, workload.StepPatch, tc.err))

			wantReason := "ReconciliationRetryScheduled"
			if tc.wantPermanent {
				wantReason = "ReconciliationFailed"
			}
			if got := v.Events[member].Reason; got != wantReason {
				t.Errorf("event = %q, want %q", got, wantReason)
			}
			if got := l.MayRun(member); got != tc.wantPermanent {
				t.Errorf("MayRun = %v, want %v", got, tc.wantPermanent)
			}
		})
	}
}

// Each transient failure doubles the wait, up to the cap, however many
// attempts pile up; the event names the time the wait ends, and the next pass
// is due then.
func TestSettle_TransientBackoffGrowsToItsCap(t *testing.T) {
	l, clk := onFakeClock()
	member := deployment("web")
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for range 200 {
		want = append(want, ledger.MaxRetryDelay)
	}
	for i, delay := range want {
		v := settle(l, web, failedOn(member, workload.StepPatch, unavailable))
		if got := l.RequeueAfter("p", time.Hour); got != delay {
			t.Fatalf("attempt %d: requeue after %v, want %v", i+1, got, delay)
		}
		if due := clk.Now().Add(delay).Format(time.RFC3339); !strings.Contains(v.Events[member].Message, due) {
			t.Errorf("attempt %d: event %q does not name the retry at %s", i+1, v.Events[member].Message, due)
		}
		clk.SetTime(clk.Now().Add(delay - time.Second))
		if l.MayRun(member) {
			t.Fatalf("attempt %d: may run a second before its retry is due", i+1)
		}
		clk.SetTime(clk.Now().Add(time.Second))
		if !l.MayRun(member) {
			t.Fatalf("attempt %d: may not run once its retry is due", i+1)
		}
	}
}

// The backoff grows with consecutive transient failures only: one after a run
// of permanent ones is a first attempt.
func TestSettle_PermanentFailuresDoNotGrowTheBackoff(t *testing.T) {
	l, clk := onFakeClock()
	member := deployment("web")
	settle(l, web, failedOn(member, workload.StepPatch, unavailable))
	for range 3 {
		settle(l, web, failedOn(member, workload.StepPatch, forbidden))
	}

	v := settle(l, web, failedOn(member, workload.StepPatch, unavailable))

	if got := l.RequeueAfter("p", time.Hour); got != ledger.BaseRetryDelay {
		t.Errorf("requeue after %v, want %v", got, ledger.BaseRetryDelay)
	}
	due := clk.Now().Add(ledger.BaseRetryDelay).Format(time.RFC3339)
	if want := "Pod recycle failed: " + unavailable.Error() + ". Attempt 1 failed, retrying at " + due; v.Events[member].Message != want {
		t.Errorf("event = %q, want %q", v.Events[member].Message, want)
	}
}

func TestSettle_PermanentFailureEvent(t *testing.T) {
	l, _ := onFakeClock()
	member := deployment("web")

	v := settle(l, web, failedOn(member, workload.StepStore, forbidden))

	want := "Recommendation could not be stored for the webhook: " + forbidden.Error() + ". Permanent error, retried every reconcile without backoff"
	if got := v.Events[member].Message; got != want {
		t.Errorf("event = %q, want %q", got, want)
	}
}

// The next pass is due at the earliest transient retry among the members the
// Policy claims, never at a permanent failure's, and never at once.
func TestRequeueAfter_TheEarliestRetry(t *testing.T) {
	l, clk := onFakeClock()
	first, second := deployment("first"), deployment("second")
	denied, healthy := deployment("denied"), deployment("healthy")
	settle(l, web, failedOn(second, workload.StepPatch, unavailable))
	clk.SetTime(clk.Now().Add(10 * time.Second))
	settle(l, web,
		failedOn(first, workload.StepPatch, unavailable), resultOf(second, ledger.Skipped),
		failedOn(denied, workload.StepPatch, forbidden), measured(healthy, 1, 0))

	if got := l.RequeueAfter("p", time.Hour); got != 20*time.Second {
		t.Errorf("requeue after %v, want 20s: second's, which failed first", got)
	}
	if got := l.RequeueAfter("p", 5*time.Second); got != 5*time.Second {
		t.Errorf("requeue after %v, want the shorter interval", got)
	}
	if got := l.RequeueAfter("other", time.Hour); got != time.Hour {
		t.Errorf("a Policy claiming none of them requeues after %v, want the interval", got)
	}

	settle(l, web, resultOf(first, ledger.Skipped), resultOf(denied, ledger.Skipped), measured(healthy, 1, 0))
	if got := l.RequeueAfter("p", time.Hour); got != ledger.BaseRetryDelay {
		t.Errorf("requeue after %v once second is no member, want first's %v", got, ledger.BaseRetryDelay)
	}

	clk.SetTime(clk.Now().Add(time.Hour))
	if got := l.RequeueAfter("p", time.Hour); got != time.Second {
		t.Errorf("requeue after %v with a retry already due, want a second", got)
	}

	settle(l, web, resultOf(denied, ledger.Skipped), measured(healthy, 1, 0))
	if got := l.RequeueAfter("p", time.Hour); got != time.Hour {
		t.Errorf("requeue after %v with only a permanent failure and a healthy member, want the interval", got)
	}
}

// A pass that cannot list its workloads requeues at the retry its Policy's
// last pass scheduled, but a retry already due waits for a pass that can
// list: no sooner than a first retry would, nor later than the interval.
func TestRequeueAfterListFailure(t *testing.T) {
	l, clk := onFakeClock()
	member := deployment("web")
	if got := l.RequeueAfterListFailure("p", time.Hour); got != time.Hour {
		t.Errorf("requeue after %v with nothing claimed, want the interval", got)
	}
	settle(l, web, failedOn(member, workload.StepPatch, unavailable))
	settle(l, web, failedOn(member, workload.StepPatch, unavailable))

	if got := l.RequeueAfterListFailure("p", time.Hour); got != 2*ledger.BaseRetryDelay {
		t.Errorf("requeue after %v, want the scheduled retry %v", got, 2*ledger.BaseRetryDelay)
	}
	clk.SetTime(clk.Now().Add(time.Hour))
	if got := l.RequeueAfterListFailure("p", time.Hour); got != ledger.BaseRetryDelay {
		t.Errorf("requeue after %v with a retry already due, want %v", got, ledger.BaseRetryDelay)
	}
	if got := l.RequeueAfterListFailure("p", ledger.BaseRetryDelay/3); got != ledger.BaseRetryDelay/3 {
		t.Errorf("requeue after %v, want the shorter interval", got)
	}
}

func TestSettle_PodCounts(t *testing.T) {
	member := deployment("web")
	before := workload.PodCounts{Total: 3, Stale: 1}
	for _, tc := range []struct {
		name         string
		res          ledger.Result
		wantMeasured bool
		wantPods     workload.PodCounts
	}{
		{"applying measures", measured(member, 3, 0), true, workload.PodCounts{Total: 3}},
		{"nothing to apply drops them", resultOf(member, ledger.Idle), false, workload.PodCounts{}},
		{"a failed OnCreate count keeps them", resultOf(member, ledger.Uncounted), true, before},
		{"a skipped member keeps them", resultOf(member, ledger.Skipped), true, before},
		{"a failed member keeps them", failedOn(member, workload.StepPatch, unavailable), true, before},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := onFakeClock()
			settle(l, web, measured(member, before.Total, before.Stale))

			got := settle(l, web, tc.res).Identities[web]

			if got.Measured != tc.wantMeasured || got.Pods != tc.wantPods {
				t.Errorf("pods = %+v (measured %v), want %+v (measured %v)", got.Pods, got.Measured, tc.wantPods, tc.wantMeasured)
			}
		})
	}
}

// An identity's health is one over all of its members: pod counts summed, a
// member skipped or failed since contributing its last ones; Blocked on the
// step of the first Blocked member in name order; attempts counting the
// members that failed in the pass. Blocked counts identities, not members.
func TestSettle_IdentityHealthAggregatesItsMembers(t *testing.T) {
	l, _ := onFakeClock()
	blue, green := deployment("api-blue"), deployment("api-green")
	pass := func(blueRes, greenRes ledger.Result) ledger.Verdict {
		return l.Settle("p", map[promclient.WorkloadIdentity][]ledger.Result{
			api: {greenRes, blueRes},
			web: {measured(deployment("web"), 1, 0)},
		})
	}

	v := pass(measured(blue, 2, 2), measured(green, 1, 1))
	if got := v.Identities[api]; got != (ledger.Health{Pods: workload.PodCounts{Total: 3, Stale: 3}, Measured: true}) {
		t.Errorf("health = %+v, want 3 pods (2 blue + 1 green), all stale", got)
	}

	v = pass(failedOn(blue, workload.StepPatch, unavailable), failedOn(green, workload.StepPrometheus, unavailable))
	got := v.Identities[api]
	if got.BlockedOn != workload.StepPatch {
		t.Errorf("blocked on %q, want the first blocked member's step (api-blue: patch)", got.BlockedOn)
	}
	if got.Pods.Total != 3 || got.Pods.Stale != 3 || got.Attempts != 2 {
		t.Errorf("health = %+v, want the last counts kept and 2 attempts", got)
	}
	if v.Blocked() != 1 {
		t.Errorf("Blocked() = %d, want 1 identity (not 2 members)", v.Blocked())
	}
	if len(v.Events) != 2 {
		t.Errorf("events = %v, want one per failed member", v.Events)
	}

	v = pass(resultOf(blue, ledger.Skipped), resultOf(green, ledger.Skipped))
	if got := v.Identities[api]; got.BlockedOn != workload.StepPatch || got.Pods.Total != 3 || got.Attempts != 0 || len(v.Events) != 0 {
		t.Errorf("health = %+v, events %v: skipped members keep their state, and fail nothing this pass", got, v.Events)
	}
}

// A member the Policy's pass no longer reports is forgotten: its backoff
// neither holds it back nor schedules a pass any more.
func TestSettle_ForgetsTheMembersNoPolicyClaims(t *testing.T) {
	l, _ := onFakeClock()
	kept, gone := deployment("kept"), deployment("gone")
	settle(l, web, measured(kept, 1, 0), failedOn(gone, workload.StepPatch, unavailable))

	settle(l, web, measured(kept, 1, 0))

	if !l.MayRun(gone) {
		t.Error("a member no Policy claims is still backed off")
	}
	if got := l.RequeueAfter("p", time.Hour); got != time.Hour {
		t.Errorf("requeue after %v, want the interval: the only retry was a forgotten member's", got)
	}
	if got := settle(l, web, measured(kept, 1, 0), resultOf(gone, ledger.Skipped)).Identities[web]; got.Blocked() {
		t.Errorf("health = %+v: a member back after being forgotten starts afresh", got)
	}
}

// An identity moving between two Policies keeps one health while both claim
// it, its members unioned; a member is forgotten only once no Policy claims
// it, whether the old Policy's pass leaves it out or the Policy is forgotten.
func TestSettle_UnionsTheMembersEveryPolicyClaims(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(l *ledger.Ledger)
	}{
		{"its pass leaves it out", func(l *ledger.Ledger) {
			l.Settle("old", map[promclient.WorkloadIdentity][]ledger.Result{})
		}},
		{"it is forgotten", func(l *ledger.Ledger) { l.Forget("old") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := onFakeClock()
			stuck, moved := deployment("web-old"), deployment("web-new")
			l.Settle("old", map[promclient.WorkloadIdentity][]ledger.Result{
				web: {measured(stuck, 2, 0)},
			})
			l.Settle("old", map[promclient.WorkloadIdentity][]ledger.Result{
				web: {failedOn(stuck, workload.StepPatch, forbidden)},
			})

			v := l.Settle("new", map[promclient.WorkloadIdentity][]ledger.Result{web: {measured(moved, 1, 1)}})
			want := ledger.Health{Pods: workload.PodCounts{Total: 3, Stale: 1}, Measured: true, BlockedOn: workload.StepPatch}
			if got := v.Identities[web]; got != want {
				t.Errorf("health while both claim it = %+v, want %+v", got, want)
			}

			tc.release(l)

			v = l.Settle("new", map[promclient.WorkloadIdentity][]ledger.Result{web: {measured(moved, 1, 1)}})
			want = ledger.Health{Pods: workload.PodCounts{Total: 1, Stale: 1}, Measured: true}
			if got := v.Identities[web]; got != want {
				t.Errorf("health once only the new Policy claims it = %+v, want %+v", got, want)
			}
			if got := l.Settle("old", map[promclient.WorkloadIdentity][]ledger.Result{
				web: {resultOf(stuck, ledger.Skipped)},
			}).Identities[web]; got.Blocked() {
				t.Errorf("health = %+v: the member was not forgotten once no Policy claimed it", got)
			}
		})
	}
}
