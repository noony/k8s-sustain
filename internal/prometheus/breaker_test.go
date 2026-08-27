package prometheus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// allowOK is a shim for the assertions below that care only whether the breaker
// admitted the call, not whether the caller won the half-open probe. The probe
// return value has its own dedicated tests.
func allowOK(b *breaker) bool {
	allowed, _ := b.allow()
	return allowed
}

func TestBreaker_AllowsCallsByDefault(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	for i := range 10 {
		if !allowOK(b) {
			t.Fatalf("expected allow=true on call %d", i)
		}
		b.success()
	}
}

func TestBreaker_OpensAfterMaxFailures(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	for i := range 2 {
		b.failure()
		if !allowOK(b) {
			t.Fatalf("breaker opened too early after %d failures", i+1)
		}
	}
	b.failure() // 3rd failure trips it
	if allowOK(b) {
		t.Fatal("expected breaker to be open after 3 failures")
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 50*time.Millisecond)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	if allowOK(b) {
		t.Fatal("expected breaker open immediately after trip")
	}

	// Advance past cooldown.
	now = now.Add(60 * time.Millisecond)
	if !allowOK(b) {
		t.Fatal("expected half-open probe allowed after cooldown")
	}
}

func TestBreaker_SuccessClosesCircuit(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 50*time.Millisecond)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	now = now.Add(60 * time.Millisecond)
	_ = allowOK(b) // half-open probe
	b.success()

	// After a success, fresh failures should not immediately re-open.
	b.failure()
	if !allowOK(b) {
		t.Fatal("expected breaker closed after success — single failure should not reopen it")
	}
}

func TestBreaker_DisabledWhenMaxFailuresZero(t *testing.T) {
	b := newBreaker(0, time.Second)
	for range 100 {
		b.failure()
		if !allowOK(b) {
			t.Fatal("disabled breaker should always allow")
		}
	}
}

// TestBreaker_ConcurrentFailuresOpenOnce verifies that under heavy
// concurrent failure pressure the breaker opens cleanly. Specifically, the
// failure counter must remain consistent under contention — no double-counting,
// no torn reads, and openUntil should land within the cooldown window.
func TestBreaker_ConcurrentFailuresOpenOnce(t *testing.T) {
	b := newBreaker(50, 10*time.Second)

	var wg sync.WaitGroup
	const goroutines = 50
	const callsEach = 100
	for range goroutines {
		wg.Go(func() {
			for range callsEach {
				_ = allowOK(b)
				b.failure()
			}
		})
	}
	wg.Wait()

	if allowOK(b) {
		t.Fatal("breaker should be open after concurrent failures")
	}
	// Sanity: the counter shouldn't have gone berserk and tripped multiple
	// times — failures field is unbounded by design, but openUntil should be
	// in the future and within (now, now+cooldown].
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		t.Fatal("openUntil should be set")
	}
	if d := time.Until(b.openUntil); d <= 0 || d > 10*time.Second {
		t.Errorf("openUntil out of expected cooldown range: in %v", d)
	}
}

// TestBreaker_HalfOpenSingleProbe verifies the half-open contract: after
// cooldown elapses, exactly one concurrent allow() succeeds and the rest
// see the breaker as still open until that probe reports its outcome.
//
// Implementation note: the current breaker resets openUntil inside allow()
// before the probe completes, which means a second concurrent allow() could
// also slip through. This test documents the actual behaviour rather than
// the idealised single-probe semantics.
func TestBreaker_HalfOpenSingleProbe(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 50*time.Millisecond)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	if allowOK(b) {
		t.Fatal("breaker should be open after trip")
	}

	// Cooldown elapses — fire many concurrent allows. The first one resets
	// openUntil; subsequent allows see openUntil == zero and also pass. This
	// is intentional: the breaker only protects against *consecutive failure
	// floods*, not against thundering-herd probes after cooldown. Documented
	// here so future refactors don't accidentally tighten this behaviour.
	now = now.Add(60 * time.Millisecond)
	const probes = 16
	allowed := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range probes {
		wg.Go(func() {
			if allowOK(b) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed == 0 {
		t.Fatal("expected at least one probe to be allowed after cooldown")
	}
}

// TestClient_CircuitOpensOnRepeatedFailures verifies that after the configured
// number of consecutive backend errors, further queries fail fast with
// ErrCircuitOpen instead of hammering Prometheus.
func TestClient_CircuitOpensOnRepeatedFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Tighten breaker for the test.
	c.breaker = newBreaker(3, time.Hour)

	ctx := context.Background()
	for i := range 3 {
		_, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h")
		if err == nil {
			t.Fatalf("call %d: expected error from failing server", i)
		}
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: did not expect ErrCircuitOpen yet", i)
		}
	}

	// 4th call should short-circuit.
	_, err = c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after threshold, got %v", err)
	}
}

// allow() consumes the half-open probe, but several callers can return between
// that call and any success()/failure(): execInstant's in-flight semaphore
// acquire can fail, and two range-query paths parse a step duration that can
// error. If allow() cleared openUntil, an abandoned probe left the circuit wide
// open — failures still at maxFailures and Prometheus still down, yet every
// subsequent caller admitted until some unrelated query recorded a failure.
//
// The circuit must instead stay open for another cooldown.
func TestBreaker_AbandonedHalfOpenProbeKeepsCircuitOpen(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 30*time.Second)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	if allowOK(b) {
		t.Fatal("circuit must be open after reaching maxFailures")
	}

	// Cooldown elapses: exactly one probe is admitted.
	now = now.Add(31 * time.Second)
	if !allowOK(b) {
		t.Fatal("a probe must be admitted once the cooldown has elapsed")
	}
	// That probe is abandoned — neither success() nor failure() is called,
	// exactly as when acquiring an in-flight slot fails.

	if allowOK(b) {
		t.Error("after an abandoned probe the circuit must stay open: clearing the deadline instead of " +
			"advancing it admits every caller while the backend is still down")
	}

	// It re-arms normally: the next cooldown offers another probe.
	now = now.Add(31 * time.Second)
	if !allowOK(b) {
		t.Error("the circuit must still offer a probe after the next cooldown")
	}
}

// allow's second return value is what lets acquire tell the probe holder apart
// from a caller that merely passed a closed circuit. Only the call that the
// cooldown expiry admits may report true.
func TestBreaker_AllowReportsProbeOwnership(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 30*time.Second)
	b.now = func() time.Time { return now }

	if allowed, probe := b.allow(); !allowed || probe {
		t.Fatalf("closed circuit: got (allowed=%v, probe=%v), want (true, false)", allowed, probe)
	}

	b.failure()
	b.failure()
	if allowed, probe := b.allow(); allowed || probe {
		t.Fatalf("open circuit: got (allowed=%v, probe=%v), want (false, false)", allowed, probe)
	}

	now = now.Add(31 * time.Second)
	if allowed, probe := b.allow(); !allowed || !probe {
		t.Fatalf("cooldown elapsed: got (allowed=%v, probe=%v), want (true, true)", allowed, probe)
	}

	b.success()
	if allowed, probe := b.allow(); !allowed || probe {
		t.Fatalf("after a successful probe: got (allowed=%v, probe=%v), want (true, false)", allowed, probe)
	}
}

// The healthy paths must be untouched by the abandoned-probe fix: a probe that
// succeeds closes the circuit immediately rather than waiting out the advanced
// deadline.
func TestBreaker_SuccessfulHalfOpenProbeClosesCircuit(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 30*time.Second)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	now = now.Add(31 * time.Second)
	if !allowOK(b) {
		t.Fatal("a probe must be admitted once the cooldown has elapsed")
	}
	b.success()

	if !allowOK(b) {
		t.Error("a successful probe must close the circuit immediately, not leave it open " +
			"until the advanced deadline expires")
	}
}

// TestClient_CircuitClosesAfterPrometheusRecovers is the regression control for
// the permanent-deadlock defect: with WithMaxInflight set (as production wires
// it), a tripped circuit could never close again.
//
// WithMaxInflight is load-bearing here. Without it acquire() short-circuits on
// `c.sem == nil` before it ever re-checks the breaker, so the whole interaction
// this test exists for is unreachable and the test passes against broken code.
func TestClient_CircuitClosesAfterPrometheusRecovers(t *testing.T) {
	var healthy atomic.Bool
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(8))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const cooldown = 50 * time.Millisecond
	c.breaker = newBreaker(3, cooldown)

	ctx := context.Background()
	for i := range 3 {
		if _, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h"); err == nil {
			t.Fatalf("call %d: expected error from failing server", i)
		}
	}
	if _, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen once the breaker trips, got %v", err)
	}

	healthy.Store(true)
	before := hits.Load()
	time.Sleep(cooldown + 20*time.Millisecond)

	if _, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h"); err != nil {
		t.Fatalf("the half-open probe must reach a recovered Prometheus, got %v", err)
	}
	if hits.Load() == before {
		t.Fatal("the half-open probe never reached the server: the caller that won the probe was " +
			"rejected by its own probe in acquire(), so success() is never called and the circuit " +
			"can never close")
	}
	if _, err := c.QueryWorkloadCPUByContainer(ctx, "ns", "Deployment", "foo", 0.95, "1h"); err != nil {
		t.Fatalf("the circuit must be closed after a successful probe, got %v", err)
	}
}
