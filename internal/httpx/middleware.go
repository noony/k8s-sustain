package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-logr/logr"
)

// PanicCounter is the optional callback the recovery middleware invokes when
// it catches a panic. Pass nil to skip metric accounting.
type PanicCounter func(path string)

// LatencyObserver is invoked once per request with the request's path,
// http.StatusText(status), and total handler duration. Pass nil to skip
// metric accounting.
type LatencyObserver func(path, status string, duration time.Duration)

// RouteLabeler maps an *http.Request to a low-cardinality label suitable for
// Prometheus. It's called after the inner handler runs so callers can rely on
// router-populated fields like r.Pattern (set by http.ServeMux in Go 1.22+).
// Implementations MUST return a bounded set of values — the raw URL path is
// attacker-controlled and will blow up label cardinality.
//
// Returning "" is allowed and gets normalized to "unknown".
type RouteLabeler func(r *http.Request) string

// DefaultRouteLabeler returns r.Pattern when the request matched a registered
// http.ServeMux pattern, falling back to "unknown" otherwise. This keeps the
// catch-all SPA mount ("/") from collapsing onto attacker-controlled paths
// while still surfacing meaningful labels for API routes that have an
// explicit pattern (e.g. "GET /api/policies/{name}").
func DefaultRouteLabeler(r *http.Request) string {
	if r == nil || r.Pattern == "" {
		return "unknown"
	}
	return r.Pattern
}

// routeLabel normalizes a nil labeler or an empty result to "unknown" so metric
// labels stay bounded.
func routeLabel(r *http.Request, labelRoute RouteLabeler) string {
	if labelRoute != nil {
		if v := labelRoute(r); v != "" {
			return v
		}
	}
	return "unknown"
}

// WithRequestID accepts an inbound X-Request-Id or generates one, then exposes
// it on the response headers, on the request context, and (via WriteJSON /
// WriteError) in the response body's meta.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(RequestIDHeader)
		if rid == "" {
			rid = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithRecovery turns a handler panic into a 500 response and a structured
// log entry instead of crashing the process. Sits inside WithTelemetry so a
// recovered request still gets observed with its final 500 status.
//
// When count is non-nil it is invoked once per recovered panic with the route
// label (derived from labelRoute, which defaults to "unknown" when nil or
// when the function returns ""). Passing the raw URL path here would let an
// attacker explode the panic counter's label cardinality just by triggering
// crashes against bogus paths.
func WithRecovery(next http.Handler, logger logr.Logger, count PanicCounter, labelRoute RouteLabeler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the stdlib's signal to drop the
			// connection silently — propagate it so net/http's own handler
			// sees the panic instead of treating it as a 500.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			logger.Error(
				nil, "http handler panic",
				"panic", fmt.Sprint(rec),
				"path", r.URL.Path,
				"method", r.Method,
				"requestId", RequestIDFromContext(r.Context()),
				"stack", string(debug.Stack()),
			)
			if count != nil {
				count(routeLabel(r, labelRoute))
			}
			WriteError(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// statusResponseWriter captures the status code so WithTelemetry can record
// it in the request_duration histogram and in the access log.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush passes through to the underlying writer so streaming endpoints
// (Prometheus query-range responses, SSE) keep working even when telemetry
// wraps them.
func (rw *statusResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WithTelemetry records request duration and emits a verbose access-log line.
// Pass a nil observe to log without exporting a metric.
//
// labelRoute runs after the inner handler so it can read router-populated
// fields. Passing nil collapses everything onto "unknown" — fine for tests, but
// never pass the raw URL path in production: on the dashboard's SPA catch-all
// it is attacker-controlled and an OOM footgun against Prometheus.
func WithTelemetry(next http.Handler, logger logr.Logger, observe LatencyObserver, labelRoute RouteLabeler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		if observe != nil {
			observe(routeLabel(r, labelRoute), http.StatusText(rw.statusCode), dur)
		}
		logger.V(1).Info(
			"http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", dur.String(),
			"requestId", RequestIDFromContext(r.Context()),
			"remote", r.RemoteAddr,
		)
	})
}
