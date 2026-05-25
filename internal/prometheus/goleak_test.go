package prometheus

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. The circuit
// breaker uses time-based state transitions and the HTTP client retries
// concurrently — a leak here means a request goroutine outlived its test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
