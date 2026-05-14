package dashboard

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// ---- Request ID ----

// withRequestID accepts an inbound X-Request-Id (so a frontend can stitch
// together its own correlation chain) or generates one. The value is then
// available three ways:
//
//   - on the response headers (echoed back to the client)
//   - on the request context for handlers
//   - through writeJSON / writeError which copy it into the response body's
//     meta so a UI error report can include it without inspecting headers
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(requestIDHeader)
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set(requestIDHeader, rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---- CORS ----

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default: no CORS headers — same-origin only. Operators must opt
		// into cross-origin access by listing trusted origins (or "*"
		// explicitly) in --cors-allowed-origins.
		origin := ""
		switch {
		case len(s.CORSOrigins) == 0:
			// same-origin only
		case len(s.CORSOrigins) == 1 && s.CORSOrigins[0] == "*":
			origin = "*"
		default:
			reqOrigin := r.Header.Get("Origin")
			for _, o := range s.CORSOrigins {
				if o == reqOrigin {
					origin = o
					break
				}
			}
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Recovery ----

// withRecovery turns a handler panic into a 500 response and a structured
// log entry instead of crashing the dashboard process. Sits inside
// withTelemetry so the recovered request still gets observed with its
// final 500 status.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.Logger.Error(nil, "dashboard handler panic",
				"panic", fmt.Sprint(rec),
				"path", r.URL.Path,
				"method", r.Method,
				"requestId", requestIDFromContext(r.Context()),
				"stack", string(debug.Stack()),
			)
			panicTotal.WithLabelValues(r.URL.Path).Inc()
			writeError(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- Telemetry ----

// telemetryResponseWriter captures the status code so withTelemetry can
// record it in the request_duration histogram and in the access log.
type telemetryResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *telemetryResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *Server) withTelemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &telemetryResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		requestDuration.WithLabelValues(r.URL.Path, http.StatusText(rw.statusCode)).Observe(dur.Seconds())
		s.Logger.V(1).Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", dur.String(),
			"requestId", requestIDFromContext(r.Context()),
			"remote", r.RemoteAddr,
		)
	})
}

// ---- Gzip ----

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter wraps http.ResponseWriter so handler-written bytes get
// transparently compressed. It is intentionally light: it does not buffer
// to decide whether compression is worthwhile — JSON responses from this
// API are uniformly large enough (time series, paginated lists) that
// compressing unconditionally is the right default.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	h := g.Header()
	if h.Get("Content-Encoding") == "" {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		// Length of the original payload no longer matches what's on the
		// wire; let the runtime use chunked transfer instead.
		h.Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(status)
	g.wroteHeader = true
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withGzip compresses responses when the client advertises gzip support.
// The wrapper is no-op on requests that don't ask for it, so older HTTP
// clients (curl without --compressed, our integration tests) still get
// plain JSON.
func (s *Server) withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipWriterPool.Put(gz)
		}()
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(grw, r)
	})
}
