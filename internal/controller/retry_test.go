package controller

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRetryTracker_ShouldSkip_NoEntry(t *testing.T) {
	rt := newRetryTracker()
	if rt.shouldSkip("Deployment/prod/web") {
		t.Error("should not skip unknown workload")
	}
}

func TestRetryTracker_RecordFailure_ThenSkip(t *testing.T) {
	rt := newRetryTracker()
	rt.recordFailure("Deployment/prod/web")

	if !rt.shouldSkip("Deployment/prod/web") {
		t.Error("should skip after failure")
	}
}

func TestRetryTracker_RecordSuccess_ClearsEntry(t *testing.T) {
	rt := newRetryTracker()
	rt.recordFailure("Deployment/prod/web")
	rt.recordSuccess("Deployment/prod/web")

	if rt.shouldSkip("Deployment/prod/web") {
		t.Error("should not skip after success")
	}
}

func TestRetryTracker_ExponentialBackoff(t *testing.T) {
	rt := newRetryTracker()

	rt.recordFailure("Deployment/prod/web")
	state1 := rt.getState("Deployment/prod/web")
	delay1 := time.Until(state1.nextRetry)

	rt.recordFailure("Deployment/prod/web")
	state2 := rt.getState("Deployment/prod/web")
	delay2 := time.Until(state2.nextRetry)

	if delay2 <= delay1 {
		t.Errorf("expected increasing delay, got %v then %v", delay1, delay2)
	}
	if state2.attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", state2.attempts)
	}
}

func TestRetryTracker_MaxBackoff(t *testing.T) {
	rt := newRetryTracker()
	for range 20 {
		rt.recordFailure("Deployment/prod/web")
	}
	state := rt.getState("Deployment/prod/web")
	delay := time.Until(state.nextRetry)
	if delay > maxRetryDelay+time.Second {
		t.Errorf("delay %v exceeds max %v", delay, maxRetryDelay)
	}
}

// TestRetryTracker_MaxBackoff_NoOverflow guards against int64 overflow in the
// exponential backoff shift. With baseRetryDelay=30s, `1 << (attempts-1)` starts
// overflowing time.Duration once attempts ≈ 30, and becomes undefined/zero once
// attempts >= 64. Drive the counter well past both thresholds and assert the
// computed delay stays clamped to [0, maxRetryDelay].
func TestRetryTracker_MaxBackoff_NoOverflow(t *testing.T) {
	rt := newRetryTracker()
	const key = "Deployment/prod/web"
	for i := range 200 {
		before := time.Now()
		rt.recordFailure(key)
		state := rt.getState(key)
		delay := state.nextRetry.Sub(before)
		if delay < 0 {
			t.Fatalf("iteration %d: negative delay %v (overflow corrupted backoff)", i, delay)
		}
		if delay > maxRetryDelay+time.Second {
			t.Fatalf("iteration %d (attempts=%d): delay %v exceeds max %v", i, state.attempts, delay, maxRetryDelay)
		}
	}
}

func TestRetryTracker_RemoveSilently(t *testing.T) {
	rt := newRetryTracker()
	rt.recordFailure("Deployment/prod/web")
	rt.remove("Deployment/prod/web")

	if rt.shouldSkip("Deployment/prod/web") {
		t.Error("should not skip after removal")
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"not found", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "web"), false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "web", nil), false},
		{"server timeout", apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "list", 5), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 5), true},
		{"service unavailable", apierrors.NewServiceUnavailable("down"), true},
		{"internal error", apierrors.NewInternalError(fmt.Errorf("oops")), true},
		{"generic error", fmt.Errorf("prometheus query failed"), true},
		{"wrapped not found", fmt.Errorf("wrap: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "web")), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err); got != tt.transient {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.transient)
			}
		})
	}
}

// Verify unused import guard — net/http is used only to reference http.StatusTooManyRequests
// in the test table name, but it's actually used in retry.go. The import here is for
// the apierrors.NewTooManyRequests helper. Keeping the import clean.
var _ = http.StatusOK

func TestBlockedCountAmong(t *testing.T) {
	rt := newRetryTracker()
	keys := []string{
		"Deployment/prod/web",
		"Deployment/prod/api",
		"Deployment/prod/worker",
	}
	// Two of the three workloads are in backoff; the third has no entry.
	rt.recordFailure(keys[0])
	rt.recordFailure(keys[1])

	if got := rt.blockedCountAmong(keys); got != 2 {
		t.Errorf("blockedCountAmong: got %d, want 2", got)
	}
}

// TestRetryTracker_RecordFailure_ReturnsResultingState verifies recordFailure
// hands back the state it just computed under the lock. handleStepError needs
// the attempt count and next-retry time it produced; reading them back with a
// second, separate getState call left a window in which a concurrent
// recordSuccess could delete the entry, so getState returned nil and the
// caller panicked dereferencing it.
func TestRetryTracker_RecordFailure_ReturnsResultingState(t *testing.T) {
	rt := newRetryTracker()
	const key = "Deployment/prod/web"

	first := rt.recordFailure(key)
	if first == nil {
		t.Fatal("recordFailure must return the resulting state, got nil")
	}
	if first.attempts != 1 {
		t.Errorf("attempts = %d, want 1", first.attempts)
	}
	if first.nextRetry.IsZero() {
		t.Error("nextRetry must be set")
	}

	second := rt.recordFailure(key)
	if second.attempts != 2 {
		t.Errorf("attempts = %d, want 2", second.attempts)
	}
	if !second.nextRetry.After(first.nextRetry) {
		t.Errorf("nextRetry did not advance: %v then %v", first.nextRetry, second.nextRetry)
	}

	// The returned value is a copy: mutating it must not corrupt the tracker.
	second.attempts = 99
	if state := rt.getState(key); state == nil || state.attempts != 2 {
		t.Errorf("returned state must be a copy, tracker now holds %+v", state)
	}
}

// TestRetryTracker_RecordFailure_RacesRecordSuccess drives the exact
// interleaving that used to panic the operator: the same workload key
// dispatched to two goroutines (which a duplicated selector namespace made
// possible), one recording a transient failure while the other records
// success and deletes the entry. recordFailure must always return usable
// state; nothing here may nil-deref.
func TestRetryTracker_RecordFailure_RacesRecordSuccess(t *testing.T) {
	rt := newRetryTracker()
	const key = "Deployment/prod/web"

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			state := rt.recordFailure(key)
			if state == nil {
				t.Error("recordFailure returned nil state")
				return
			}
			_ = state.attempts
			_ = state.nextRetry
		}()
		go func() {
			defer wg.Done()
			rt.recordSuccess(key)
		}()
	}
	wg.Wait()
}
