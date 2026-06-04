package httpx

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
// so the wider value is safe for it too. Callers that genuinely need a
// different value for a specific field can override it via an Option.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 15 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Option mutates an *http.Server during NewServer construction. It follows the
// same functional-options idiom used by internal/prometheus and
// internal/workload so the package style stays consistent.
type Option func(*http.Server)

// WithReadHeaderTimeout overrides the default ReadHeaderTimeout.
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(s *http.Server) { s.ReadHeaderTimeout = d }
}

// WithReadTimeout overrides the default ReadTimeout.
func WithReadTimeout(d time.Duration) Option {
	return func(s *http.Server) { s.ReadTimeout = d }
}

// WithWriteTimeout overrides the default WriteTimeout.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *http.Server) { s.WriteTimeout = d }
}

// WithIdleTimeout overrides the default IdleTimeout.
func WithIdleTimeout(d time.Duration) Option {
	return func(s *http.Server) { s.IdleTimeout = d }
}

// NewServer builds an *http.Server bound to addr with handler installed and
// the hardened timeout defaults above applied to every timeout field. Pass
// Options to override individual fields; pass none to get the shared defaults.
//
// The caller still owns ListenAndServe / Shutdown (see
// ListenAndServeWithShutdown) and any TLSConfig wiring — NewServer only
// centralizes construction and timeout hardening, not the listen lifecycle.
func NewServer(addr string, handler http.Handler, opts ...Option) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
	for _, opt := range opts {
		opt(srv)
	}
	return srv
}

// ListenAndServeWithShutdown runs listen in a goroutine and blocks until
// either the listener returns an error or the process receives SIGTERM /
// SIGINT. On signal, it calls srv.Shutdown with the supplied timeout and
// returns the result.
//
// name appears in the "Shutting down …" log line so the operator can tell
// which server is going down when several share the process.
//
// listen is a closure rather than a srv method so callers can pick between
// ListenAndServe and ListenAndServeTLS without httpx growing TLS knobs.
func ListenAndServeWithShutdown(
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		logger.Info("Shutting down " + name + " server")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
