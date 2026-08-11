package httpx

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// Hardened http.Server timeout defaults shared by every server in the
// process. These are the single source of truth so the webhook and dashboard
// can no longer drift apart by hand.
//
// ReadTimeout/WriteTimeout are set to the more generous of the two historical
// values (the dashboard's 15s, not the webhook's old 10s) so the dashboard's
// larger SPA + API responses are never truncated. The webhook serves tiny
// AdmissionReview JSON well within this budget, and its real deadline is still
// enforced upstream by the apiserver's MutatingWebhookConfiguration timeout,
// so the wider value is safe for it too.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 15 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// NewServer builds an *http.Server bound to addr with handler installed and
// the hardened timeout defaults above applied to every timeout field.
//
// The caller still owns ListenAndServe / Shutdown (see
// ListenAndServeWithShutdown) and any TLSConfig wiring — NewServer only
// centralizes construction and timeout hardening, not the listen lifecycle.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
}

// ListenAndServeWithShutdown runs listen in a goroutine and blocks until
// either the listener returns an error or ctx is cancelled. On cancellation it
// calls srv.Shutdown with the supplied timeout and returns the result.
//
// Shutdown is driven by ctx rather than by a signal handler registered here,
// so the CALLER owns the process's one signal source. That matters because a
// server is rarely the only thing that needs to stop: the webhook also runs an
// informer cache and a cert watcher, and those must outlive the HTTP drain —
// requests are still being served during it. When this function installed its
// own signal.Notify alongside the caller's ctrl.SetupSignalHandler, the two
// fired concurrently and the cache could stop mid-drain, leaving late
// admissions reading a store that had stopped updating. One context, cancelled
// by one handler, makes that ordering the caller's explicit choice instead of a
// race between two handlers.
//
// name appears in the "Shutting down …" log line so the operator can tell
// which server is going down when several share the process.
//
// listen is a closure rather than a srv method so callers can pick between
// ListenAndServe and ListenAndServeTLS without httpx growing TLS knobs.
func ListenAndServeWithShutdown(
	ctx context.Context,
	srv *http.Server,
	logger logr.Logger,
	name string,
	shutdownTimeout time.Duration,
	listen func() error,
) error {
	errCh := make(chan error, 1)
	go func() {
		if err := listen(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("Shutting down " + name + " server")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
