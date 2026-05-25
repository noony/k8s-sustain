package dashboard

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. The dashboard
// spawns parallel Prometheus queries from several handlers and runs an
// in-memory cache; a leak here means a goroutine outlived the request
// context that owned it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
