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

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return httpx.WithRequestID(next)
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return httpx.WithRecovery(next, s.Logger, func(path string) {
		panicTotal.WithLabelValues(path).Inc()
	}, httpx.DefaultRouteLabeler)
}

func (s *Server) withTelemetry(next http.Handler) http.Handler {
	return httpx.WithTelemetry(next, s.Logger, func(path, status string, dur time.Duration) {
		requestDuration.WithLabelValues(path, status).Observe(dur.Seconds())
	}, httpx.DefaultRouteLabeler)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No CORS headers unless --cors-allowed-origins opts in.
		reqOrigin := r.Header.Get("Origin")
		origin := ""
		switch {
		case len(s.CORSOrigins) == 0:
			// same-origin only
		case len(s.CORSOrigins) == 1 && s.CORSOrigins[0] == "*":
			origin = "*"
		default:
			for _, o := range s.CORSOrigins {
				if o == reqOrigin {
					origin = o
					break
				}
			}
		}
		// Without Vary: Origin a shared cache can serve origin-A's allow header
		// to origin-B. Add rather than Set to keep the gzip wrapper's Vary.
		if reqOrigin != "" && len(s.CORSOrigins) > 0 {
			w.Header().Add("Vary", "Origin")
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

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// passthroughStatus reports whether status must carry no body (RFC 9110:
// 204, 304, 1xx). Gzip framing on a 304 makes browsers fail with
// ERR_CONTENT_DECODING_FAILED.
func passthroughStatus(status int) bool {
	return status == http.StatusNoContent ||
		status == http.StatusNotModified ||
		(status >= 100 && status < 200)
}

// alreadyCompressedType reports whether a Content-Type is already compressed
// at rest, so re-compressing would be wasted CPU.
func alreadyCompressedType(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		// SVG is XML text and compresses well.
		return ct != "image/svg+xml"
	case strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"):
		return true
	case ct == "application/zip",
		ct == "application/gzip",
		ct == "application/x-gzip",
		ct == "application/x-brotli",
		ct == "application/x-7z-compressed":
		return true
	}
	return false
}

// gzipResponseWriter compresses handler-written bytes unconditionally, and
// flips into passthrough for bodiless statuses and already-encoded content.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	passthrough bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	h := g.Header()

	if passthroughStatus(status) {
		g.passthrough = true
		h.Del("Content-Encoding")
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	if h.Get("Content-Length") == "0" {
		g.passthrough = true
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	if h.Get("Content-Encoding") != "" || alreadyCompressedType(h.Get("Content-Type")) {
		g.passthrough = true
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	// Content-Length no longer matches the wire; let the runtime chunk.
	h.Del("Content-Length")
	g.ResponseWriter.WriteHeader(status)
	g.wroteHeader = true
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.passthrough {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	if !g.passthrough {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withGzip compresses responses when the client advertises gzip support.
func (s *Server) withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			// Close exactly when the response was committed as gzip: closing after a
			// passthrough or a never-committed header leaks an empty-gzip trailer,
			// while a committed gzip header with no body (/healthz) still needs it.
			if grw.wroteHeader && !grw.passthrough {
				_ = gz.Close()
			} else {
				gz.Reset(io.Discard)
			}
			gzipWriterPool.Put(gz)
		}()
		next.ServeHTTP(grw, r)
	})
}
