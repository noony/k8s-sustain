package controller

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	baseRetryDelay = 30 * time.Second
	maxRetryDelay  = 5 * time.Minute

	// retryStateMaxIdle is how long after nextRetry has elapsed we keep an
	// entry around. A workload that fails, then is deleted, never calls
	// recordSuccess; without this floor the tracker would leak one entry
	// per deleted-while-failing workload across the controller's lifetime.
	// Set well above maxRetryDelay so legitimate backoff windows finish.
	retryStateMaxIdle = time.Hour

	// retryPruneInterval throttles how often we walk the map to drop
	// long-stale entries. Pruning is lazy — triggered by recordFailure —
	// to keep the package free of background goroutines.
	retryPruneInterval = 10 * time.Minute
)

type retryState struct {
	attempts  int
	nextRetry time.Time
	phase     string
}

type retryTracker struct {
	mu        sync.Mutex
	states    map[string]*retryState
	lastPrune time.Time
}

func newRetryTracker() *retryTracker {
	return &retryTracker{states: make(map[string]*retryState)}
}

// shouldSkip returns true if the workload is in backoff and should not be processed yet.
func (rt *retryTracker) shouldSkip(key string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.states[key]
	if !ok {
		return false
	}
	return time.Now().Before(s.nextRetry)
}

// recordFailure increments the attempt counter, records the failing phase and
// sets the next retry time with exponential backoff capped at maxRetryDelay.
// It returns a copy of the resulting state, never nil.
//
// Callers must use the returned value rather than a separate getState call: a
// concurrent recordSuccess for the same key can delete the entry in between, so
// the read comes back nil. Computing it under the one lock makes the pair
// atomic, and the copy keeps the caller off the map's live value.
func (rt *retryTracker) recordFailure(key, phase string) *retryState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	s, ok := rt.states[key]
	if !ok {
		s = &retryState{}
		rt.states[key] = s
	}
	s.attempts++
	s.phase = phase
	// time.Duration is an int64 of nanoseconds, so `baseRetryDelay << shift`
	// overflows for a large shift and wraps to zero once shift >= 64.
	// maxShift=16 puts 30s << 16 ≈ 23 days: far above maxRetryDelay, nowhere
	// near overflow.
	const maxShift = 16
	shift := min(s.attempts-1, maxShift)
	delay := min(baseRetryDelay<<shift, maxRetryDelay)
	s.nextRetry = now.Add(delay)
	cp := *s
	rt.pruneLocked(now)
	return &cp
}

// pruneLocked drops entries whose nextRetry is more than retryStateMaxIdle
// in the past. Caller must hold rt.mu. Throttled by retryPruneInterval so
// the walk cost is bounded even under high failure churn.
func (rt *retryTracker) pruneLocked(now time.Time) {
	if now.Sub(rt.lastPrune) < retryPruneInterval {
		return
	}
	rt.lastPrune = now
	cutoff := now.Add(-retryStateMaxIdle)
	for k, s := range rt.states {
		if s.nextRetry.Before(cutoff) {
			delete(rt.states, k)
		}
	}
}

// clear drops the retry state for the workload, after a successful step or
// when the workload is no longer retried.
func (rt *retryTracker) clear(key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.states, key)
}

// getState returns a copy of the retry state for testing. Returns nil if not found.
func (rt *retryTracker) getState(key string) *retryState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.states[key]
	if !ok {
		return nil
	}
	cp := *s
	return &cp
}

// blockedPhase reports whether the workload is blocked — its last step failed
// transiently and no step has succeeded since — and the phase that failed.
func (rt *retryTracker) blockedPhase(key string) (string, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.states[key]
	if !ok {
		return "", false
	}
	return s.phase, true
}

// isTransientError returns true for errors that should trigger a retry with backoff.
// Permanent errors (not found, invalid, context cancellation) return false.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		code := statusErr.Status().Code
		// 4xx (except 429) are permanent client errors.
		if code >= 400 && code < 500 && code != http.StatusTooManyRequests {
			return false
		}
	}
	// Everything else (Prometheus errors, 5xx, 429, network errors) is transient.
	return true
}
