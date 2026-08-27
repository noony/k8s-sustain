package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/noony/k8s-sustain/internal/config"
)

// ctrl.SetupSignalHandler REPLACES Go's default "SIGTERM terminates the
// process" with "cancel this context". Everything that can block after it is
// installed therefore has to honour that context, or the signal is swallowed
// for as long as the block lasts and the pod is only removed by the SIGKILL at
// the end of terminationGracePeriodSeconds.
//
// The startup phase is the one that blocks hardest: NewCached waits up to
// crdWaitTimeout (2m) for the CRDs to become servable, plus an unbounded
// WaitForCacheSync, and nothing answers /healthz for any of it. Reproduce with
// installCRDs=false or a CRD that never establishes, then `kubectl rollout
// restart` and watch the pod sit there. Before the informer cache existed the
// signal handler was registered inside ListenAndServeWithShutdown — i.e. only
// once the server was actually serving — so this window did not exist.
//
// The deps context deliberately does NOT track the signal (the informer cache
// and cert watcher must outlive the drain), which is exactly why the startup
// phase needs a context of its own rather than borrowing either one.
func TestServe_AbortsStartupWhenTheSignalContextIsCancelled(t *testing.T) {
	// Unblocks the stub if the assertion below fails, so a failing run does not
	// leave a goroutine parked for the rest of the package's tests.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	orig := newCachedClient
	t.Cleanup(func() { newCachedClient = orig })
	// Stands in for NewCached's blocking startup: it returns when the context
	// it was handed for that phase is cancelled, and not before.
	newCachedClient = func(_, startupCtx context.Context, _ *runtime.Scheme) (client.Client, error) {
		select {
		case <-startupCtx.Done():
			return nil, startupCtx.Err()
		case <-release:
			return nil, errors.New("test ended while the startup phase was still blocked")
		}
	}

	// SIGTERM arrives while the CRDs are still not servable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- serve(ctx, config.WebhookConfig{Port: 9443}, logr.Discard()) }()

	select {
	case err := <-done:
		// A shutdown signal during startup is not a failure: the process is
		// being asked to go away before it ever served anything.
		if err != nil {
			t.Errorf("serve returned %v, want nil: a shutdown signal during startup is an "+
				"ordinary exit, not a crash", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve is still running 5s after its context was cancelled: the startup phase " +
			"does not observe the signal context, so SIGTERM is ignored for the whole startup " +
			"window (up to crdWaitTimeout, 2m) and the pod is only removed by SIGKILL at the end " +
			"of terminationGracePeriodSeconds")
	}
}
