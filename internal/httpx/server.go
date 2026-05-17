package httpx

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

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
		if err := listen(); err != nil && err != http.ErrServerClosed {
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
