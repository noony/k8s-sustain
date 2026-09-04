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

// Regression: the startup phase blocks for up to crdWaitTimeout (2m) plus an
// unbounded WaitForCacheSync with nothing answering /healthz, so it must observe
// the signal context or SIGTERM is swallowed for that whole window. The deps
// context deliberately does not track the signal (the cache and cert watcher
// outlive the drain), which is why startup needs a context of its own.
func TestServe_AbortsStartupWhenTheSignalContextIsCancelled(t *testing.T) {
	// The cleanup must JOIN serve before newCachedClient is restored: on the
	// timeout path serve is still running, and a straggler reading that
	// package-level var would race the restore. Cleanups run LIFO, so the
	// restore is registered first and therefore runs last.
	release := make(chan struct{})
	finished := make(chan struct{})
	done := make(chan error, 1)

	orig := newCachedClient
	t.Cleanup(func() { newCachedClient = orig })
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("serve is still running after the test finished")
		}
	})
	// Stands in for NewCached's blocking startup.
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

	go func() {
		defer close(finished)
		done <- serve(ctx, config.WebhookConfig{Port: 9443}, logr.Discard())
	}()

	select {
	case err := <-done:
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
