package prometheus

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when the breaker has tripped and is shedding load.
var ErrCircuitOpen = errors.New("prometheus: circuit breaker open")

// breaker is a consecutive-failure circuit breaker with an implicit half-open
// state: after the cooldown one probe call is allowed, and its outcome closes
// or re-opens the circuit. maxFailures=0 disables the breaker entirely.
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

// allow reports whether a call may proceed, and whether this caller consumed
// the half-open probe.
//
// A probe holder must never re-check isOpen(): allow advances openUntil when it
// hands the probe out, so the holder would see the deadline it just set and
// reject itself. The probe would then never reach Prometheus, success() would
// never run, and the circuit would stay open forever. See Client.acquire.
func (b *breaker) allow() (allowed, probe bool) {
	if b == nil || b.maxFailures <= 0 {
		return true, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true, false
	}
	if b.now().Before(b.openUntil) {
		return false, false
	}
	// Advance openUntil rather than clearing it: a probe holder can return
	// without ever calling success()/failure() (semaphore acquire failure, step
	// parse errors), and clearing would then leave the circuit wide open with
	// Prometheus still down. Advancing keeps it open for one more cooldown,
	// which is self-healing and needs no release discipline from callers.
	b.openUntil = b.now().Add(b.cooldown)
	return true, true
}

// isOpen observes whether the circuit is open without consuming the half-open
// probe, for callers that already passed allow() and want to re-check after a
// delay. It is only meaningful to a caller that passed allow() while the
// circuit was closed — a probe holder always sees true (see allow).
func (b *breaker) isOpen() bool {
	if b == nil || b.maxFailures <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.openUntil.IsZero() && b.now().Before(b.openUntil)
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
