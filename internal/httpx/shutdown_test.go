package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// ListenAndServeWithShutdown used to install its own signal.Notify, making it a
// second handler in any process whose caller already had one. Both fired on the
// same SIGTERM, so the webhook's informer cache could stop mid-drain. This pins
// that cancelling ctx, and nothing else, triggers the graceful shutdown.
func TestListenAndServeWithShutdown_ShutsDownOnContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(ln.Addr().String(), http.NewServeMux())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ListenAndServeWithShutdown(ctx, srv, logr.Discard(), "test", time.Second,
			func() error { return srv.Serve(ln) })
	}()

	select {
	case err := <-done:
		t.Fatalf("returned before ctx was cancelled: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("graceful shutdown returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not shut down within 5s of ctx cancellation")
	}
}

// A listener that fails must surface immediately rather than hanging until the
// process is signalled — otherwise a port clash looks like a healthy start.
func TestListenAndServeWithShutdown_ReturnsListenError(t *testing.T) {
	srv := NewServer("127.0.0.1:0", http.NewServeMux())
	want := errors.New("listener exploded")

	done := make(chan error, 1)
	go func() {
		done <- ListenAndServeWithShutdown(context.Background(), srv, logr.Discard(), "test", time.Second,
			func() error { return want })
	}()

	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("got %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a listen error must return immediately, not wait for a shutdown signal")
	}
}

// http.ErrServerClosed is the normal result of Shutdown racing the listener; it
// must not be reported as a startup failure.
func TestListenAndServeWithShutdown_IgnoresErrServerClosed(t *testing.T) {
	srv := NewServer("127.0.0.1:0", http.NewServeMux())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ListenAndServeWithShutdown(ctx, srv, logr.Discard(), "test", time.Second,
			func() error { return http.ErrServerClosed })
	}()

	select {
	case err := <-done:
		t.Fatalf("ErrServerClosed must not end the wait: got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown returned %v, want nil", err)
	}
}
