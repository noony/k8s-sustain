package dashboard

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func metricsRegistry() interface {
	Gather() ([]*dto.MetricFamily, error)
} {
	return registryForTest
}

func TestHandlerRecordsRequestDuration(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rec, req)

	mfs, _ := metricsRegistry().Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "k8s_sustain_dashboard_request_duration_seconds" {
			found = true
		}
	}
	if !found {
		t.Fatal("dashboard duration histogram not registered")
	}
}

func TestMetricsEndpointServesRegistry(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	h := srv.Handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition", ct)
	}
	if !strings.Contains(rec.Body.String(), "k8s_sustain_dashboard_request_duration_seconds") {
		t.Error("/metrics body missing dashboard request duration histogram")
	}
}

func TestMetricsEndpointGzipIsSingleEncoded(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	h := srv.Handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(body), "k8s_sustain_dashboard_request_duration_seconds") {
		t.Error("decoded /metrics body missing dashboard request duration histogram (double-compressed?)")
	}
}

// TestCORS_DefaultIsSameOrigin verifies that an unconfigured Server does not
// emit Access-Control-Allow-Origin — the safe default after closing the
// previous wildcard fallback.
func TestCORS_DefaultIsSameOrigin(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("default config: Access-Control-Allow-Origin = %q, want empty", got)
	}
}

// TestCORS_ExplicitWildcard verifies that ["*"] still grants the wildcard
// (operators must opt in explicitly now).
func TestCORS_ExplicitWildcard(t *testing.T) {
	srv := &Server{Logger: testLogger(t), CORSOrigins: []string{"*"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://anything.example")
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestCORS_AllowList verifies that listed origins are echoed back and
// unlisted ones receive no CORS header.
func TestCORS_AllowList(t *testing.T) {
	srv := &Server{Logger: testLogger(t), CORSOrigins: []string{"https://ok.example"}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://ok.example")
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ok.example" {
		t.Errorf("listed origin: Access-Control-Allow-Origin = %q, want https://ok.example", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	srv.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin: Access-Control-Allow-Origin = %q, want empty", got)
	}
}

// TestUnmatchedAPIPathReturnsJSON404 pins the SPA-fallback fix: an /api/* path
// that matches no registered route must return the JSON 404 error envelope,
// not index.html with a 200.
func TestUnmatchedAPIPathReturnsJSON404(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/no-such-endpoint", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != ErrCodeNotFound {
		t.Errorf("error.code = %q, want %q", env.Error.Code, ErrCodeNotFound)
	}
}
