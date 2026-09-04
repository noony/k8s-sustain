package k8s

import (
	"testing"

	"go.uber.org/goleak"
)

// NewCached runs the informer cache in a goroutine whose lifetime is a context
// the CALLER owns, so a future error return added above its cleanup guard would
// leak a full informer set per failed startup. This is the backstop.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}
