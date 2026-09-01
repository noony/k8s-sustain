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

// recordFailure increments the attempt counter and sets the next retry time
// with exponential backoff capped at maxRetryDelay. It returns a copy of the
// resulting state, never nil.
//
// The return value is what callers must use to report the attempt: reading the
// state back with a separate getState call leaves a window in which another
// goroutine handling the SAME workload key can call recordSuccess and delete
// the entry, so the read comes back nil. Computing and returning it under the
// one lock makes that pair atomic, and a copy keeps the caller off the map's
// live value.
func (rt *retryTracker) recordFailure(key string) *retryState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	s, ok := rt.states[key]
	if !ok {
		s = &retryState{}
		rt.states[key] = s
	}
	s.attempts++
	// Cap the shift exponent before shifting: time.Duration is an int64 of
	// nanoseconds, so `baseRetryDelay << shift` overflows when shift is large
	// (and becomes undefined / wraps to zero once shift >= 64). maxShift=16
	// is plenty: baseRetryDelay (30s) << 16 ≈ 23 days, far above
	// maxRetryDelay, yet nowhere near int64 overflow.
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

// recordSuccess removes the retry state for the workload.
func (rt *retryTracker) recordSuccess(key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.states, key)
}

// remove silently deletes the retry state (used for deleted workloads).
func (rt *retryTracker) remove(key string) {
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

// blockedCountAmong returns the number of given keys currently in retry-backoff.
// Used to compute per-policy at-risk counts after a reconcile cycle.
func (rt *retryTracker) blockedCountAmong(keys []string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	count := 0
	for _, k := range keys {
		s, ok := rt.states[k]
		if !ok {
			continue
		}
		if now.Before(s.nextRetry) {
			count++
		}
	}
	return count
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
