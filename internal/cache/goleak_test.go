package cache

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. A read-through
// fetch runs on a goroutine detached from its callers; a leak here means one
// outlived its deadline.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
