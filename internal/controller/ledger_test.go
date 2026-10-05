package controller

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

	"github.com/noony/k8s-sustain/internal/workload"
)

var (
	unavailable = apierrors.NewServiceUnavailable("apiserver unavailable")
	forbidden   = apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"}, "web-1", errors.New("no RBAC"))
)

func ledgerOnFakeClock() (*ledger, *clocktesting.FakePassiveClock) {
	clk := clocktesting.NewFakePassiveClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	return newLedger(clk), clk
}

func TestLedger_Record(t *testing.T) {
	member := deploymentTarget("default", "web")
	failedOn := func(phase string, err error) applyResult { return failedStep(member, phase, err) }
	succeeded := func(kind applyKind) applyResult { return applyResult{member: member, kind: kind} }
	cases := []struct {
		name        string
		before      []applyResult
		res         applyResult
		shutdown    bool
		wantFailed  bool
		wantEvent   string
		wantBlocked string
		wantMayRun  bool
	}{
		{
			name: "transient failure backs off", res: failedOn(phasePatch, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: phasePatch,
		},
		{
			name: "query timeout is a transient failure", res: failedOn(phasePrometheus, fmt.Errorf("querying cpu: %w", context.DeadlineExceeded)),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: phasePrometheus,
		},
		{
			name: "every transient attempt is warned about", before: []applyResult{failedOn(phasePatch, unavailable)}, res: failedOn(phasePatch, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: phasePatch,
		},
		{
			name: "permanent failure blocks without backoff", res: failedOn(phasePatch, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: phasePatch, wantMayRun: true,
		},
		{
			name: "permanent failure is warned about once", before: []applyResult{failedOn(phasePatch, forbidden)}, res: failedOn(phasePatch, forbidden),
			wantFailed: true, wantBlocked: phasePatch, wantMayRun: true,
		},
		{
			name: "transient failure turning permanent is warned about", before: []applyResult{failedOn(phasePatch, unavailable)}, res: failedOn(phasePatch, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: phasePatch, wantMayRun: true,
		},
		{
			name: "member withheld for a failed write counts", res: failedOn(phaseStore, unavailable),
			wantFailed: true, wantEvent: "ReconciliationRetryScheduled", wantBlocked: phaseStore,
		},
		{
			name: "member withheld for a forbidden Ensure counts", res: failedOn(phaseStore, forbidden),
			wantFailed: true, wantEvent: "ReconciliationFailed", wantBlocked: phaseStore, wantMayRun: true,
		},
		{
			name: "applying clears a failure", before: []applyResult{failedOn(phasePatch, forbidden)}, res: succeeded(applyDone),
			wantMayRun: true,
		},
		{
			name: "nothing to apply clears a failure", before: []applyResult{failedOn(phasePatch, unavailable)}, res: succeeded(applyIdle),
			wantMayRun: true,
		},
		{
			name: "a failed OnCreate count clears a failure", before: []applyResult{failedOn(phasePatch, unavailable)}, res: succeeded(applyUncounted),
			wantMayRun: true,
		},
		{
			name: "a skipped member stays Blocked", before: []applyResult{failedOn(phasePatch, unavailable)}, res: succeeded(applySkipped),
			wantBlocked: phasePatch,
		},
		{
			name: "a failure at shutdown is not recorded", res: failedOn(phasePatch, unavailable), shutdown: true,
			wantMayRun: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := ledgerOnFakeClock()
			for _, res := range tc.before {
				l.record(context.Background(), res)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.shutdown {
				cancel()
			}

			ev, failed := l.record(ctx, tc.res)

			if failed != tc.wantFailed {
				t.Errorf("failed = %v, want %v", failed, tc.wantFailed)
			}
			if got := reasonOf(ev); got != tc.wantEvent {
				t.Errorf("event = %q, want %q", got, tc.wantEvent)
			}
			if got := l.health(member.key()).blockedOn; got != tc.wantBlocked {
				t.Errorf("blocked on %q, want %q", got, tc.wantBlocked)
			}
			if got := l.mayRun(member.key()); got != tc.wantMayRun {
				t.Errorf("mayRun = %v, want %v", got, tc.wantMayRun)
			}
		})
	}
}

func reasonOf(ev *ledgerEvent) string {
	if ev == nil {
		return ""
	}
	return ev.reason
}

// Each transient failure doubles the wait, up to the cap, however many
// attempts pile up; the event names the time the wait ends.
func TestLedger_TransientBackoffGrowsToItsCap(t *testing.T) {
	l, clk := ledgerOnFakeClock()
	member := deploymentTarget("default", "web")
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for range 200 {
		want = append(want, maxRetryDelay)
	}
	for i, delay := range want {
		ev, _ := l.record(context.Background(), failedStep(member, phasePatch, unavailable))
		got, ok := l.nextRetry([]string{member.key()})
		if !ok || got != delay {
			t.Fatalf("attempt %d: next retry in %v (%v), want %v", i+1, got, ok, delay)
		}
		if due := clk.Now().Add(delay).Format(time.RFC3339); !strings.Contains(ev.message, due) {
			t.Errorf("attempt %d: event %q does not name the retry at %s", i+1, ev.message, due)
		}
		clk.SetTime(clk.Now().Add(delay - time.Second))
		if l.mayRun(member.key()) {
			t.Fatalf("attempt %d: may run a second before its retry is due", i+1)
		}
		clk.SetTime(clk.Now().Add(time.Second))
		if !l.mayRun(member.key()) {
			t.Fatalf("attempt %d: may not run once its retry is due", i+1)
		}
	}
}

func TestLedger_NextRetryIsTheEarliestBackoff(t *testing.T) {
	l, clk := ledgerOnFakeClock()
	first, second := deploymentTarget("default", "first"), deploymentTarget("default", "second")
	denied, healthy := deploymentTarget("default", "denied"), deploymentTarget("default", "healthy")
	l.record(context.Background(), failedStep(second, phasePatch, unavailable))
	clk.SetTime(clk.Now().Add(10 * time.Second))
	l.record(context.Background(), failedStep(first, phasePatch, unavailable))
	l.record(context.Background(), failedStep(denied, phasePatch, forbidden))
	l.record(context.Background(), applyResult{member: healthy, kind: applyDone})

	all := []string{first.key(), second.key(), denied.key(), healthy.key()}
	if got, ok := l.nextRetry(all); !ok || got != 20*time.Second {
		t.Errorf("next retry in %v (%v), want 20s: second's, which failed first", got, ok)
	}
	if _, ok := l.nextRetry([]string{denied.key(), healthy.key()}); ok {
		t.Error("a permanent failure or a healthy member scheduled a retry")
	}
	l.forget(second.key())
	if got, ok := l.nextRetry(all); !ok || got != baseRetryDelay {
		t.Errorf("next retry in %v (%v) once second is forgotten, want %v", got, ok, baseRetryDelay)
	}
}

func TestLedger_PodCounts(t *testing.T) {
	member := deploymentTarget("default", "web")
	measured := workload.PodCounts{Total: 3, Stale: 1}
	for _, tc := range []struct {
		name         string
		res          applyResult
		wantMeasured bool
		wantPods     workload.PodCounts
	}{
		{"applying measures", applyResult{member: member, kind: applyDone, pods: workload.PodCounts{Total: 3}}, true, workload.PodCounts{Total: 3}},
		{"nothing to apply drops them", applyResult{member: member, kind: applyIdle}, false, workload.PodCounts{}},
		{"a failed OnCreate count keeps them", applyResult{member: member, kind: applyUncounted}, true, measured},
		{"a skipped member keeps them", applyResult{member: member, kind: applySkipped}, true, measured},
		{"a failed member keeps them", failedStep(member, phasePatch, unavailable), true, measured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := ledgerOnFakeClock()
			l.record(context.Background(), applyResult{member: member, kind: applyDone, pods: measured})

			l.record(context.Background(), tc.res)

			got := l.health(member.key())
			if got.measured != tc.wantMeasured || got.pods != tc.wantPods {
				t.Errorf("pods = %+v (measured %v), want %+v (measured %v)", got.pods, got.measured, tc.wantPods, tc.wantMeasured)
			}
		})
	}
}

func TestPermanentError(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"prometheus error", errors.New("prometheus query failed"), false},
		{"query timeout", fmt.Errorf("querying cpu: %w", context.DeadlineExceeded), false},
		{"server timeout", apierrors.NewServerTimeout(pods, "list", 5), false},
		{"throttled", apierrors.NewTooManyRequests("slow down", 5), false},
		{"unavailable", unavailable, false},
		{"internal error", apierrors.NewInternalError(errors.New("oops")), false},
		{"forbidden", forbidden, true},
		{"unauthorized", apierrors.NewUnauthorized("expired token"), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "web", nil), true},
		{"not found", apierrors.NewNotFound(pods, "web"), true},
		{"wrapped forbidden", fmt.Errorf("pod web-1: %w", forbidden), true},
		{"one forbidden pod among transient ones", errors.Join(
			fmt.Errorf("pod web-1: %w", unavailable), fmt.Errorf("pod web-2: %w", forbidden)), true},
		{"only transient pods", errors.Join(
			fmt.Errorf("pod web-1: %w", unavailable), fmt.Errorf("pod web-2: %w", unavailable)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentError(tc.err); got != tc.want {
				t.Errorf("permanentError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
