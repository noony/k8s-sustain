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

// WithRequestID accepts an inbound X-Request-Id (so a frontend can stitch
// together its own correlation chain) or generates one. The value is then
// available three ways:
//
//   - on the response headers (echoed back to the client)
//   - on the request context for handlers
//   - through WriteJSON / WriteError which copy it into the response body's
//     meta so a UI error report can include it without inspecting headers
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
				label := "unknown"
				if labelRoute != nil {
					if v := labelRoute(r); v != "" {
						label = v
					}
				}
				count(label)
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

// WithTelemetry records request duration and emits a verbose access-log
// line. The status histogram is supplied through observe; pass nil to log
// without exporting a metric.
//
// labelRoute is called after the inner handler has run to derive the low-
// cardinality route label that's reported to the observer. Passing nil
// collapses every request onto a single "unknown" bucket — useful for tests
// but never what you want in production, where the raw URL path (an
// attacker-controlled value on the dashboard's SPA catch-all) is an OOM
// footgun against Prometheus.
func WithTelemetry(next http.Handler, logger logr.Logger, observe LatencyObserver, labelRoute RouteLabeler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		if observe != nil {
			label := "unknown"
			if labelRoute != nil {
				if v := labelRoute(r); v != "" {
					label = v
				}
			}
			observe(label, http.StatusText(rw.statusCode), dur)
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

// LimitRequestBody installs an http.MaxBytesReader on r.Body. Apply at the
// entry of any handler that decodes a request body so an oversized payload
// is rejected before it lands in memory.
func LimitRequestBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}
