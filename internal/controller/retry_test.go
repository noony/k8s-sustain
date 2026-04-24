package controller

import (
	"context"
	"fmt"
	"net/http"
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
	for i := 0; i < 20; i++ {
		rt.recordFailure("Deployment/prod/web")
	}
	state := rt.getState("Deployment/prod/web")
	delay := time.Until(state.nextRetry)
	if delay > maxRetryDelay+time.Second {
		t.Errorf("delay %v exceeds max %v", delay, maxRetryDelay)
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
