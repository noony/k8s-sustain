package oomwatch

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. The watcher,
// cache sweeper, and trigger handler all spawn background goroutines whose
// owners are expected to cancel them on test cleanup — a leak here means a
// caller forgot to wait or to cancel its context.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
