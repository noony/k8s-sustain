package dashboard

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/noony/k8s-sustain/internal/httpx"
)

// TestGzip_NotModifiedNoEncodingNoBody pins the fix for the RFC 9110
// violation: a handler that ServeContent-returns 304 must not get
// Content-Encoding: gzip, and must not have any gzip framing in its body.
// The previous wrapper unconditionally set the header and the deferred
// gz.Close() wrote ~10 bytes of empty-gzip trailer, which browsers reported
// as ERR_CONTENT_DECODING_FAILED on reload.
func TestGzip_NotModifiedNoEncodingNoBody(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty on 304", got)
	}
	if body := rec.Body.Bytes(); len(body) != 0 {
		t.Errorf("body = %x, want empty on 304 (gzip framing must not leak)", body)
	}
}

// TestGzip_NoContentNoEncodingNoBody is the 204 analogue of the 304 case.
func TestGzip_NoContentNoEncodingNoBody(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty on 204", got)
	}
	if body := rec.Body.Bytes(); len(body) != 0 {
		t.Errorf("body = %x, want empty on 204", body)
	}
}

// TestGzip_NoWriteHandlerDoesNotLeakFraming pins the second leak: a handler
// that returns without calling Write or WriteHeader must not produce a
// response body at all. The previous deferred gz.Close() emitted ~10 bytes
// of empty-gzip header and committed an implicit 200 without
// Content-Encoding — a client would see those bytes and choke.
func TestGzip_NoWriteHandlerDoesNotLeakFraming(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// intentionally write nothing
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/noop", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if body := rec.Body.Bytes(); len(body) != 0 {
		t.Errorf("body = %x (len=%d), want empty (no gzip framing on no-write)", body, len(body))
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
}

// TestGzip_OKResponseIsCompressed sanity-checks the happy path: a real 200
// with a body still goes through gzip, the wire bytes are a valid gzip
// stream, and Content-Encoding + Vary headers are set.
func TestGzip_OKResponseIsCompressed(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	payload := strings.Repeat("hello world ", 50)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want to contain Accept-Encoding", got)
	}
	gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gr.Close() }()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("decompressed body = %q, want %q", got, payload)
	}
}

// TestGzip_AlreadyCompressedTypeBypassed verifies the small skip-list: an
// image/png response should pass through unmodified rather than getting
// pointless re-compression.
func TestGzip_AlreadyCompressedTypeBypassed(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	payload := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payload)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logo.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty on image/png", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("body = %x, want %x (raw passthrough)", rec.Body.Bytes(), payload)
	}
}

// TestCORS_VaryOriginAlwaysSetWhenOriginPresent is the regression fix for
// the missing Vary: Origin: any time a CORS-enabled response can vary by
// the request's Origin header, the middleware MUST advertise that. Without
// it, a downstream cache or CDN can serve origin-A's
// Access-Control-Allow-Origin back to origin-B and break the same-origin
// policy.
func TestCORS_VaryOriginAlwaysSetWhenOriginPresent(t *testing.T) {
	srv := &Server{
		Logger:      testLogger(t),
		CORSOrigins: []string{"https://a.example", "https://b.example"},
	}

	cases := []struct {
		name       string
		origin     string
		wantACAO   string
		wantVaryIn bool // Vary: Origin must always be present when Origin is set
	}{
		{"listed-a", "https://a.example", "https://a.example", true},
		{"listed-b", "https://b.example", "https://b.example", true},
		{"unlisted", "https://evil.example", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Header.Set("Origin", tc.origin)
			srv.Handler().ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantACAO {
				t.Errorf("ACAO = %q, want %q", got, tc.wantACAO)
			}
			vary := rec.Header().Values("Vary")
			hasOrigin := false
			for _, v := range vary {
				for part := range strings.SplitSeq(v, ",") {
					if strings.EqualFold(strings.TrimSpace(part), "Origin") {
						hasOrigin = true
					}
				}
			}
			if hasOrigin != tc.wantVaryIn {
				t.Errorf("Vary: Origin present = %v (Vary=%v), want %v", hasOrigin, vary, tc.wantVaryIn)
			}
		})
	}
}

// TestCORS_VaryOriginNotSetWhenNoAllowlist verifies we don't gratuitously
// emit Vary: Origin on the default (no-CORS) configuration — same-origin
// responses don't vary by Origin.
func TestCORS_VaryOriginNotSetWhenNoAllowlist(t *testing.T) {
	srv := &Server{Logger: testLogger(t)} // no CORSOrigins
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://anything.example")
	srv.Handler().ServeHTTP(rec, req)

	for _, v := range rec.Header().Values("Vary") {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "Origin") {
				t.Errorf("did not expect Vary: Origin when no CORSOrigins configured (Vary=%v)", rec.Header().Values("Vary"))
			}
		}
	}
}

// TestTelemetry_AccessLogCarriesRequestID pins the middleware ordering fix:
// withRequestID must sit outside withTelemetry, otherwise the access-log line
// is emitted before the request ID lands on the context and always logs
// requestId="".
func TestTelemetry_AccessLogCarriesRequestID(t *testing.T) {
	var lines []string
	log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{Verbosity: 1})
	srv := &Server{Logger: log}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(httpx.RequestIDHeader, "test-rid-123")
	srv.Handler().ServeHTTP(rec, req)

	for _, line := range lines {
		if strings.Contains(line, "http request") {
			if !strings.Contains(line, "test-rid-123") {
				t.Fatalf("access log line missing request ID: %s", line)
			}
			return
		}
	}
	t.Fatalf("no access log line emitted; lines=%v", lines)
}

// TestTelemetry_UnknownPathLabelsAsUnknown is the cardinality fix: any
// request that doesn't match a registered route pattern (i.e. an attacker
// hitting random URLs on the SPA catch-all) must land in a single
// "unknown" or pattern-bounded bucket. The raw URL path must NEVER appear
// as a histogram label.
func TestTelemetry_UnknownPathLabelsAsUnknown(t *testing.T) {
	// Build a tiny handler chain that mirrors the dashboard's mux + telemetry
	// wiring. We don't use Server.Handler() because the dashboard's catch-all
	// "/" pattern would match every request — what we want to test is the
	// fallback when nothing matches.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /known", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	observed := make([]string, 0, 3)
	recObserve := func(path, _ string, _ time.Duration) {
		observed = append(observed, path)
	}
	pipeline := httpx.WithTelemetry(mux, testLogger(t), recObserve, httpx.DefaultRouteLabeler)

	for _, p := range []string{"/known", "/this/is/not/a/route", "/another/bogus/" + strings.Repeat("x", 200)} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		pipeline.ServeHTTP(rec, req)
	}

	if len(observed) != 3 {
		t.Fatalf("observed = %v, want 3 entries", observed)
	}
	if observed[0] == "" || observed[0] == "unknown" {
		t.Errorf("matched route should expose its pattern, got %q", observed[0])
	}
	for i, label := range observed[1:] {
		if label != "unknown" {
			t.Errorf("unmatched path %d label = %q, want \"unknown\" (raw paths would explode Prom cardinality)", i+1, label)
		}
	}
}

// TestGzip_EmptyBody200EmitsDecodableStream pins a framing bug reached
// through a different door than the 204/304 cases above: a handler that
// calls WriteHeader(200) and writes NO body (GET /healthz) still gets
// Content-Encoding: gzip committed onto the wire, but the deferred close
// used to be gated on "the handler wrote at least one byte" — so nothing at
// all was emitted. The client saw a 200 that claims to be gzip with a
// zero-byte payload, and gzip.NewReader on it fails with EOF: the same
// ERR_CONTENT_DECODING_FAILED class the passthrough logic exists to prevent.
// A response committed as gzip must always carry the ~20 bytes of empty-gzip
// framing that make it a valid, empty gzip stream.
func TestGzip_EmptyBody200EmitsDecodableStream(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/empty", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader on a response committed as gzip: %v (body = %x, len=%d)", err, rec.Body.Bytes(), rec.Body.Len())
	}
	defer func() { _ = gr.Close() }()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("decompressed body = %q, want empty", got)
	}
}

// TestGzip_HealthzRoundTripDecodes is the real-route regression for the same
// bug: /healthz is exactly the "WriteHeader, no body" shape, and it is what a
// gzip-aware prober (curl --compressed, a browser, a kubelet-style probe)
// actually hits. It goes through the full middleware chain rather than
// withGzip alone.
func TestGzip_HealthzRoundTripDecodes(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.Handler().ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader on /healthz: %v (body = %x, len=%d)", err, rec.Body.Bytes(), rec.Body.Len())
	}
	defer func() { _ = gr.Close() }()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("decompressed /healthz body = %q, want empty", got)
	}
}

// TestGzip_ContentLengthZeroPassthroughNoFraming guards the other side of the
// same fix: a handler that explicitly declares an empty body stays in
// passthrough mode, so it must get neither Content-Encoding: gzip nor any
// gzip framing bytes. Emitting the terminator for every committed header —
// rather than only for committed-as-gzip ones — would break this.
func TestGzip_ContentLengthZeroPassthroughNoFraming(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/empty-declared", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	srv.withGzip(inner).ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty on a declared-empty body", got)
	}
	if body := rec.Body.Bytes(); len(body) != 0 {
		t.Errorf("body = %x, want empty (no gzip framing in passthrough)", body)
	}
}
