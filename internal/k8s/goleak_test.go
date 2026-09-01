package k8s

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain catches goroutines that escape individual tests. NewCached runs the
// informer cache in a goroutine whose lifetime is a context the CALLER owns,
// which makes "the function cleaned up after its own failures" a property no
// single assertion inside a test can be trusted to cover forever — a future
// error return added above the cleanup guard would leak a full informer set
// per failed startup, silently, in a webhook that retries construction on
// CrashLoopBackOff.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
