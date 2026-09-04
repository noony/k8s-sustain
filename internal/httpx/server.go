package httpx

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// Hardened http.Server timeout defaults shared by every server in the process,
// sized for the dashboard's larger SPA + API responses. The webhook's own
// deadline is enforced upstream by the MutatingWebhookConfiguration timeout, so
// the wider budget is safe for it too.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 15 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// NewServer builds an *http.Server bound to addr with the hardened timeout
// defaults applied. The caller still owns the listen lifecycle (see
// ListenAndServeWithShutdown) and any TLSConfig wiring.
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

// ListenAndServeWithShutdown runs listen in a goroutine and blocks until the
// listener errors or ctx is cancelled, then calls srv.Shutdown with the given
// timeout. name appears in the shutdown log line.
//
// Shutdown is driven by ctx rather than a signal handler registered here so the
// CALLER owns the process's one signal source. When this function installed its
// own signal.Notify alongside the webhook's ctrl.SetupSignalHandler, both fired
// on the same SIGTERM and the informer cache could stop mid-drain, leaving late
// admissions reading a store that had stopped updating.
//
// listen is a closure so callers can pick ListenAndServe or ListenAndServeTLS
// without httpx growing TLS knobs.
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
