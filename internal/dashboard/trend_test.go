package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// recordingPromClient records every QueryRange expression seen so tests can
// assert all six series are queried.
type recordingPromClient struct {
	fakePromClient
	mu    sync.Mutex
	exprs []string
}

// failingRangePromClient fails QueryRange for every expression containing one
// of failSubstrings (or all expressions when the list is empty).
type failingRangePromClient struct {
	fakePromClient
	failSubstrings []string
}

func (f *failingRangePromClient) QueryRange(_ context.Context, expr, _, _ string) ([]promclient.TimeValue, error) {
	if len(f.failSubstrings) == 0 {
		return nil, errors.New("prom unreachable")
	}
	for _, sub := range f.failSubstrings {
		if strings.Contains(expr, sub) {
			return nil, errors.New("prom unreachable")
		}
	}
	return []promclient.TimeValue{{Timestamp: time.Unix(1, 0), Value: 1}}, nil
}

func (r *recordingPromClient) QueryRange(_ context.Context, expr, _, _ string) ([]promclient.TimeValue, error) {
	r.mu.Lock()
	r.exprs = append(r.exprs, expr)
	r.mu.Unlock()
	return []promclient.TimeValue{{Timestamp: time.Unix(1, 0), Value: 1}}, nil
}

func TestHandleSummaryTrendReturnsUsageRequestOriginalForCPUAndMemory(t *testing.T) {
	prom := &recordingPromClient{}
	srv := &Server{PromClient: prom, Logger: testLogger(t)}
	srv.summaryCache = NewCache(2, 60*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/api/summary/trend?window=30d", nil)
	rec := httptest.NewRecorder()
	srv.handleSummaryTrend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp trendResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	for _, s := range []struct {
		name string
		s    trendSeries
	}{{"cpu", resp.CPU}, {"memory", resp.Memory}} {
		if len(s.s.Usage) == 0 || len(s.s.Request) == 0 || len(s.s.OriginalRequest) == 0 {
			t.Fatalf("%s: missing series in response: %+v", s.name, s.s)
		}
	}

	// Verify the six trend queries are issued. Usage and current-request
	// queries are scoped to managed workloads (joined with the template
	// metric), so we match by required substrings rather than full string
	// equality — that keeps the test stable if the join formatting changes.
	type matcher struct {
		name     string
		mustHave []string
	}
	wantExprs := []matcher{
		{"cpu original", []string{"sum(k8s_sustain_workload_template_cpu_cores)"}},
		{"cpu usage scoped", []string{"k8s_sustain:workload_cpu_usage:cores", "and on", "k8s_sustain_workload_template_cpu_cores"}},
		{"cpu request scoped", []string{"k8s_sustain:container_cpu_requests_by_workload:cores", "and on", "k8s_sustain_workload_template_cpu_cores"}},
		{"mem original", []string{"sum(k8s_sustain_workload_template_memory_bytes)"}},
		{"mem usage scoped", []string{"k8s_sustain:workload_memory_usage:bytes", "and on", "k8s_sustain_workload_template_memory_bytes"}},
		{"mem request scoped", []string{"k8s_sustain:container_memory_requests_by_workload:bytes", "and on", "k8s_sustain_workload_template_memory_bytes"}},
	}
	for _, m := range wantExprs {
		seen := false
		for _, e := range prom.exprs {
			ok := true
			for _, sub := range m.mustHave {
				if !strings.Contains(e, sub) {
					ok = false
					break
				}
			}
			if ok {
				seen = true
				break
			}
		}
		if !seen {
			t.Errorf("expected %s query not issued; got %v", m.name, prom.exprs)
		}
	}
}

// TestHandleSummaryTrendAllQueriesFailReturns503 pins the outage contract: when
// every Prometheus query fails, the handler must not return a 200 with empty
// series (indistinguishable from a cluster with no data).
func TestHandleSummaryTrendAllQueriesFailReturns503(t *testing.T) {
	srv := &Server{PromClient: &failingRangePromClient{}, Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.handleSummaryTrend(rec, httptest.NewRequest(http.MethodGet, "/api/summary/trend", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want empty on error responses", got)
	}
}

// TestHandleSummaryTrendPartialFailureStillReturnsData verifies graceful
// degradation: if only some queries fail, the surviving series are served.
func TestHandleSummaryTrendPartialFailureStillReturnsData(t *testing.T) {
	// Fail only the CPU template-scoped queries; memory queries succeed.
	prom := &failingRangePromClient{failSubstrings: []string{"template_cpu_cores"}}
	srv := &Server{PromClient: prom, Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.handleSummaryTrend(rec, httptest.NewRequest(http.MethodGet, "/api/summary/trend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp trendResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Memory.Usage) == 0 {
		t.Errorf("memory.usage empty, want surviving series on partial failure")
	}
	if len(resp.CPU.OriginalRequest) != 0 {
		t.Errorf("cpu.originalRequest = %v, want empty for failed query", resp.CPU.OriginalRequest)
	}
}
