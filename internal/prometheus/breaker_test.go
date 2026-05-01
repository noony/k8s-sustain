package prometheus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBreaker_AllowsCallsByDefault(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	for i := range 10 {
		if !b.allow() {
			t.Fatalf("expected allow=true on call %d", i)
		}
		b.success()
	}
}

func TestBreaker_OpensAfterMaxFailures(t *testing.T) {
	b := newBreaker(3, 100*time.Millisecond)
	for i := range 2 {
		b.failure()
		if !b.allow() {
			t.Fatalf("breaker opened too early after %d failures", i+1)
		}
	}
	b.failure() // 3rd failure trips it
	if b.allow() {
		t.Fatal("expected breaker to be open after 3 failures")
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Now()
	b := newBreaker(2, 50*time.Millisecond)
	b.now = func() time.Time { return now }

	b.failure()
	b.failure()
	if b.allow() {
		t.Fatal("expected breaker open immediately after trip")
	}

	// Advance past cooldown.
	now = now.Add(60 * time.Millisecond)
	if !b.allow() {
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
	_ = b.allow() // half-open probe
	b.success()

	// After a success, fresh failures should not immediately re-open.
	b.failure()
	if !b.allow() {
		t.Fatal("expected breaker closed after success — single failure should not reopen it")
	}
}

func TestBreaker_DisabledWhenMaxFailuresZero(t *testing.T) {
	b := newBreaker(0, time.Second)
	for range 100 {
		b.failure()
		if !b.allow() {
			t.Fatal("disabled breaker should always allow")
		}
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
		_, err := c.QueryReplicaCountMedian(ctx, "ns", "Deployment", "foo", "1h")
		if err == nil {
			t.Fatalf("call %d: expected error from failing server", i)
		}
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: did not expect ErrCircuitOpen yet", i)
		}
	}

	// 4th call should short-circuit.
	_, err = c.QueryReplicaCountMedian(ctx, "ns", "Deployment", "foo", "1h")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after threshold, got %v", err)
	}
}
