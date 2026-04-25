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
)

type retryState struct {
	attempts  int
	nextRetry time.Time
}

type retryTracker struct {
	mu     sync.Mutex
	states map[string]*retryState
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
// with exponential backoff capped at maxRetryDelay.
func (rt *retryTracker) recordFailure(key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.states[key]
	if !ok {
		s = &retryState{}
		rt.states[key] = s
	}
	s.attempts++
	delay := baseRetryDelay * (1 << (s.attempts - 1))
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	s.nextRetry = time.Now().Add(delay)
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
