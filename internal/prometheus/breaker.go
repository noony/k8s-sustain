package prometheus

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when the breaker has tripped and is shedding load.
// Callers can detect this with errors.Is to choose fast-fail behaviour
// (e.g. skip this reconcile and retry on the next tick).
var ErrCircuitOpen = errors.New("prometheus: circuit breaker open")

// breaker is a simple consecutive-failure circuit breaker.
//
// State machine:
//   - closed: queries flow; each failure increments a counter.
//   - open: failures reached MaxFailures; all queries are rejected with
//     ErrCircuitOpen until Cooldown elapses.
//   - half-open (implicit): after cooldown, the next call is allowed; a
//     success closes the circuit, a failure re-opens it for another cooldown.
//
// MaxFailures=0 disables the breaker (Allow always returns true).
type breaker struct {
	maxFailures int
	cooldown    time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time // injectable for tests
}

func newBreaker(maxFailures int, cooldown time.Duration) *breaker {
	return &breaker{
		maxFailures: maxFailures,
		cooldown:    cooldown,
		now:         time.Now,
	}
}

// allow reports whether a call may proceed. When the circuit is open,
// returns false until the cooldown elapses.
func (b *breaker) allow() bool {
	if b == nil || b.maxFailures <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if b.now().Before(b.openUntil) {
		return false
	}
	// Cooldown elapsed — half-open: allow one probe. Reset openUntil so a
	// concurrent caller doesn't race past the same probe; failures will
	// re-open the circuit.
	b.openUntil = time.Time{}
	return true
}

// success resets the failure counter and closes the circuit.
func (b *breaker) success() {
	if b == nil || b.maxFailures <= 0 {
		return
	}
	b.mu.Lock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.mu.Unlock()
}

// failure records a failed call. Once consecutive failures reach MaxFailures
// the circuit opens for the configured cooldown.
func (b *breaker) failure() {
	if b == nil || b.maxFailures <= 0 {
		return
	}
	b.mu.Lock()
	b.failures++
	if b.failures >= b.maxFailures {
		b.openUntil = b.now().Add(b.cooldown)
	}
	b.mu.Unlock()
}
