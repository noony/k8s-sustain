package httpx

import (
	"context"
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
// When count is non-nil it is invoked once per recovered panic with the URL
// path so the caller can drive a Prometheus counter.
func WithRecovery(next http.Handler, logger logr.Logger, count PanicCounter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			logger.Error(nil, "http handler panic",
				"panic", fmt.Sprint(rec),
				"path", r.URL.Path,
				"method", r.Method,
				"requestId", RequestIDFromContext(r.Context()),
				"stack", string(debug.Stack()),
			)
			if count != nil {
				count(r.URL.Path)
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

// WithTelemetry records request duration and emits a verbose access-log
// line. The status histogram is supplied through observe; pass nil to log
// without exporting a metric.
func WithTelemetry(next http.Handler, logger logr.Logger, observe LatencyObserver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		if observe != nil {
			observe(r.URL.Path, http.StatusText(rw.statusCode), dur)
		}
		logger.V(1).Info("http request",
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
