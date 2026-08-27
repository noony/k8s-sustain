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
// allowed is false until the cooldown elapses.
//
// probe reports whether THIS caller is the one that consumed the half-open
// probe — i.e. the cooldown had just elapsed and this call is the single
// attempt the circuit is granting before it decides to close or re-open. It is
// false on the ordinary closed-circuit path and false when the call is
// rejected.
//
// The distinction exists because allow() ADVANCES openUntil when it hands out
// the probe (see below), so a probe holder that re-checks isOpen() afterwards
// would see the deadline IT just set and reject itself. That is a permanent
// deadlock, not a race: the probe never reaches Prometheus, success() is never
// called, and the circuit stays open for every subsequent cooldown too. Callers
// that re-check must therefore skip the check when probe is true — see
// Client.acquire.
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
	// Cooldown elapsed — half-open: allow one probe. ADVANCE openUntil by
	// another cooldown rather than clearing it. Both forms stop a concurrent
	// caller racing past the same probe, but only this one is safe when the
	// probe never reports back.
	//
	// allow() consumes the probe, yet several callers can return between here
	// and any success()/failure(): execInstant's in-flight semaphore acquire
	// can fail, and two range-query paths parse a step duration that can error.
	// Clearing openUntil in those cases left the circuit wide open — failures
	// still at maxFailures, Prometheus still down, but every subsequent caller
	// admitted until some unrelated query happened to record a failure.
	//
	// Advancing instead makes the abandoned case self-healing: the circuit
	// simply stays open for one more cooldown, which is the safe direction and
	// needs no release discipline from callers. The healthy paths are
	// unaffected — success() clears openUntil outright, and failure() re-arms
	// it to the same value this line sets.
	b.openUntil = b.now().Add(b.cooldown)
	return true, true
}

// isOpen reports whether the circuit is currently open, WITHOUT consuming the
// half-open probe that allow() would.
//
// The distinction matters: allow() is a claim ("I am the one call going
// through this cooldown"), whereas isOpen is an observation ("is it still worth
// proceeding?"). Callers that already passed allow() and simply want to
// re-check after a delay — acquire(), after a long wait for an in-flight slot —
// must use this, or each re-check would burn a probe the backend never actually
// gets.
//
// It is only a meaningful observation for a caller that passed allow() while
// the circuit was CLOSED. A caller holding the half-open probe always sees this
// return true, because allow() advanced openUntil to hand the probe out; such a
// caller must not consult isOpen at all (see allow's probe return value).
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
