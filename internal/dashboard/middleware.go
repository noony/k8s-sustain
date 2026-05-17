package dashboard

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/noony/k8s-sustain/internal/httpx"
)

// ---- Request ID / Recovery / Telemetry: delegate to internal/httpx ----

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return httpx.WithRequestID(next)
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return httpx.WithRecovery(next, s.Logger, func(path string) {
		panicTotal.WithLabelValues(path).Inc()
	})
}

func (s *Server) withTelemetry(next http.Handler) http.Handler {
	return httpx.WithTelemetry(next, s.Logger, func(path, status string, dur time.Duration) {
		requestDuration.WithLabelValues(path, status).Observe(dur.Seconds())
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
