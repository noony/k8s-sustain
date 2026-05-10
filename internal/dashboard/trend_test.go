package dashboard

import (
	"context"
	"encoding/json"
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
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
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
