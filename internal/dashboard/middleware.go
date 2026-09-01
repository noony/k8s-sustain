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
	}, httpx.DefaultRouteLabeler)
}

func (s *Server) withTelemetry(next http.Handler) http.Handler {
	return httpx.WithTelemetry(next, s.Logger, func(path, status string, dur time.Duration) {
		requestDuration.WithLabelValues(path, status).Observe(dur.Seconds())
	}, httpx.DefaultRouteLabeler)
}

// ---- CORS ----

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default: no CORS headers — same-origin only. Operators must opt
		// into cross-origin access by listing trusted origins (or "*"
		// explicitly) in --cors-allowed-origins.
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
		// Advertise that the response varies by Origin whenever the request
		// carries one AND a CORS allowlist is configured. Without Vary:
		// Origin, a downstream cache or CDN that key on URL + Cache-Control
		// alone can serve origin-A's Access-Control-Allow-Origin header to
		// a request from origin-B, breaking the browser's same-origin policy.
		// Append (Add) rather than overwrite so a handler-set Vary entry
		// (e.g. Accept-Encoding from the gzip wrapper) is preserved.
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

// ---- Gzip ----

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// passthroughStatus reports whether the response status MUST NOT carry a
// body, per RFC 9110 §6.4.1 (204, 304, all 1xx). For those statuses the
// gzip wrapper bypasses compression entirely: setting Content-Encoding: gzip
// and then emitting the ~10 bytes of empty-gzip framing onto a 304 (returned
// by http.ServeContent for a fresh index.html reload) makes the browser
// surface ERR_CONTENT_DECODING_FAILED.
func passthroughStatus(status int) bool {
	return status == http.StatusNoContent ||
		status == http.StatusNotModified ||
		(status >= 100 && status < 200)
}

// alreadyCompressedType reports whether a Content-Type is for a payload
// that's already compressed at rest. Re-compressing is CPU for no gain and
// occasionally hurts (PNGs, MP4 chunks). The skip list is intentionally
// short — the dashboard serves JSON and a tiny embedded UI bundle; this is
// defense-in-depth for whatever asset types the SPA build happens to ship.
func alreadyCompressedType(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		// SVG is XML text and compresses very well — keep it on.
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

// gzipResponseWriter wraps http.ResponseWriter so handler-written bytes get
// transparently compressed. It is intentionally light: it does not buffer
// to decide whether compression is worthwhile — JSON responses from this
// API are uniformly large enough (time series, paginated lists) that
// compressing unconditionally is the right default.
//
// A few responses MUST NOT carry a body (204, 304, 1xx) and a few content
// types are already compressed — the writer flips into passthrough mode for
// those so we don't ship gzip framing bytes the client will reject.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	passthrough bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	h := g.Header()

	// RFC 9110: 204, 304, and 1xx must carry no body. Going gzip on these
	// produces a 304 with a 10-byte empty-gzip trailer that the browser
	// then fails to decode (ERR_CONTENT_DECODING_FAILED on reload).
	if passthroughStatus(status) {
		g.passthrough = true
		h.Del("Content-Encoding")
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	// Handler explicitly declared an empty body — passthrough so we don't
	// inject gzip framing where there's nothing to encode.
	if h.Get("Content-Length") == "0" {
		g.passthrough = true
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	// Handler already encoded the body (pre-compressed static asset, or
	// upstream-proxied response) — don't double-encode. Same for content
	// types that are already compressed.
	if h.Get("Content-Encoding") != "" || alreadyCompressedType(h.Get("Content-Type")) {
		g.passthrough = true
		g.ResponseWriter.WriteHeader(status)
		g.wroteHeader = true
		return
	}

	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	// Length of the original payload no longer matches what's on the wire;
	// let the runtime use chunked transfer instead.
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
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			// Close (and so emit the gzip stream terminator) exactly when the
			// response was COMMITTED as gzip — the header went out carrying
			// Content-Encoding: gzip — rather than when the handler happened
			// to write a byte.
			//
			// Both edges matter. A handler that never committed a header at
			// all, or one that picked passthrough, must not leak the
			// ~10-byte empty-gzip trailer onto the wire: that would
			// implicitly commit a 200 the handler never sent, and the client
			// would see garbage with no Content-Encoding header to tell it
			// how to decode the bytes. But a handler that committed a gzip
			// header and then wrote NO body (WriteHeader(200) alone — the
			// /healthz shape) must still get the framing: gating on
			// "wrote at least one byte" left such a response advertising
			// Content-Encoding: gzip over a zero-byte payload, which any
			// gzip-aware client fails to decode (EOF) — the same
			// ERR_CONTENT_DECODING_FAILED class passthroughStatus exists to
			// prevent, reached through a different door.
			if grw.wroteHeader && !grw.passthrough {
				_ = gz.Close()
			} else {
				// Drop any buffered state before returning to the pool.
				gz.Reset(io.Discard)
			}
			gzipWriterPool.Put(gz)
		}()
		next.ServeHTTP(grw, r)
	})
}
