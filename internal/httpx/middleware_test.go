package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
)

// TestDefaultRouteLabeler_UnmatchedPathIsUnknown is the cardinality fix:
// when a request didn't match a registered ServeMux pattern (r.Pattern is
// empty), the labeler must emit a single bounded value rather than echo
// back the attacker-controlled URL path.
func TestDefaultRouteLabeler_UnmatchedPathIsUnknown(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/whatever/random/path", nil)
	// r.Pattern is zero on a synthetic request — what the middleware sees
	// for any URL that doesn't hit a registered handler.
	if got := DefaultRouteLabeler(r); got != "unknown" {
		t.Errorf("DefaultRouteLabeler unmatched = %q, want %q", got, "unknown")
	}
}

// TestDefaultRouteLabeler_MatchedPathUsesPattern verifies that requests
// which DID match a pattern get the pattern back — that's the whole point
// of having low-cardinality labels.
func TestDefaultRouteLabeler_MatchedPathUsesPattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/policies/{name}", func(w http.ResponseWriter, r *http.Request) {
		// r.Pattern is set by the mux when the request enters this handler.
		if got := DefaultRouteLabeler(r); got != "GET /api/policies/{name}" {
			t.Errorf("DefaultRouteLabeler matched = %q, want %q", got, "GET /api/policies/{name}")
		}
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/policies/example", nil)
	mux.ServeHTTP(rec, req)
}

// TestWithTelemetry_LabelsViaRouter pins the end-to-end wiring: after the
// inner handler runs, WithTelemetry must read r.Pattern (set by the mux) to
// label the histogram. The previous implementation passed r.URL.Path
// directly, blowing up Prometheus label cardinality on the dashboard's SPA
// catch-all.
func TestWithTelemetry_LabelsViaRouter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /known", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /also-known/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var labels []string
	observe := func(path, _ string, _ time.Duration) {
		labels = append(labels, path)
	}
	pipeline := WithTelemetry(mux, testr.New(t), observe, DefaultRouteLabeler)

	cases := []struct {
		method, url, want string
	}{
		{http.MethodGet, "/known", "GET /known"},
		{http.MethodGet, "/also-known/42", "GET /also-known/{id}"},
		{http.MethodGet, "/also-known/99", "GET /also-known/{id}"}, // same bucket
		{http.MethodGet, "/totally/random/" + "abcdef", "unknown"},
		{http.MethodPost, "/known", "unknown"}, // method mismatch -> 405, no pattern set
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.url, nil)
		pipeline.ServeHTTP(rec, req)
	}

	if len(labels) != len(cases) {
		t.Fatalf("got %d labels, want %d (%v)", len(labels), len(cases), labels)
	}
	for i, tc := range cases {
		if labels[i] != tc.want {
			t.Errorf("case %d (%s %s): label = %q, want %q", i, tc.method, tc.url, labels[i], tc.want)
		}
	}
}

// TestWithRecovery_PanicLabelIsBounded pins the analogous fix for the
// panic counter. The previous implementation passed r.URL.Path to the
// counter callback; a single attacker hitting bogus paths could explode
// the metric.
func TestWithRecovery_PanicLabelIsBounded(t *testing.T) {
	var seen []string
	count := func(path string) { seen = append(seen, path) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("kaboom")
	})

	pipeline := WithRecovery(mux, testr.New(t), count, DefaultRouteLabeler)

	// Hit the boom route directly: r.Pattern is set, so the label should be
	// the bounded pattern.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	pipeline.ServeHTTP(rec, req)

	if len(seen) != 1 {
		t.Fatalf("count callback invoked %d times, want 1", len(seen))
	}
	if seen[0] != "GET /boom" {
		t.Errorf("panic label = %q, want %q (bounded route pattern)", seen[0], "GET /boom")
	}
}
