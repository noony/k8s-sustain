package webhook

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. The webhook
// runs admission handlers with errgroup fan-out and a TLS cert watcher; a
// leak here means a worker outlived the admission request that owned it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
