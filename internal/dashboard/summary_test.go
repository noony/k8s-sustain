package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// testLogger returns a logr.Logger that writes to the test's log.
func testLogger(t *testing.T) logr.Logger { return testr.New(t) }

// fakePromClient satisfies the PromQuerier interface for tests.
// It returns canned values from `instant` for QueryInstant and
// from `byLabel` (keyed by expression) for QueryByLabel. Other
// methods return zero values so they don't break the build.
type fakePromClient struct {
	instant    map[string]float64
	byLabel    map[string]map[string]float64
	byLabels   map[string]map[string]float64
	instantErr map[string]error
	byLabelErr map[string]error
}

func (f *fakePromClient) QueryInstant(_ context.Context, expr string) (float64, error) {
	if err, ok := f.instantErr[expr]; ok {
		return 0, err
	}
	return f.instant[expr], nil
}

func (f *fakePromClient) QueryByLabel(_ context.Context, expr, _ string) (map[string]float64, error) {
	if err, ok := f.byLabelErr[expr]; ok {
		return nil, err
	}
	if v, ok := f.byLabel[expr]; ok {
		return v, nil
	}
	return map[string]float64{}, nil
}

func (f *fakePromClient) QueryByLabels(_ context.Context, expr string, _ ...string) (map[string]float64, error) {
	if v, ok := f.byLabels[expr]; ok {
		return v, nil
	}
	return map[string]float64{}, nil
}

func (f *fakePromClient) QueryRange(_ context.Context, _, _, _ string) ([]promclient.TimeValue, error) {
	return nil, nil
}

func (f *fakePromClient) Ping(_ context.Context) error { return nil }

func (f *fakePromClient) QueryCPUByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
	return promclient.ContainerValues{}, nil
}

func (f *fakePromClient) QueryMemoryByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
	return promclient.ContainerValues{}, nil
}

func (f *fakePromClient) QueryCPURangeByContainer(_ context.Context, _, _, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRangeByContainer(_ context.Context, _, _, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryCPURequestRangeByContainer(_ context.Context, _, _, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRequestRangeByContainer(_ context.Context, _, _, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryCPURecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _, _, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryOOMKillEvents(_ context.Context, _, _, _, _, _ string) ([]promclient.OOMEvent, error) {
	return nil, nil
}

func TestHandleSummaryShape(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{
			"k8s_sustain:cluster_cpu_savings_cores":    3.2,
			"k8s_sustain:cluster_cpu_savings_ratio":    0.18,
			"k8s_sustain:cluster_memory_savings_bytes": 4096,
			"k8s_sustain:cluster_memory_savings_ratio": 0.25,
		},
	}

	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Logger:     testr.New(t),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got summaryResponseV2
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v body=%s", err, rec.Body.String())
	}

	if got.KPI.CPUSavedCores != 3.2 {
		t.Errorf("kpi.cpuSavedCores = %v, want 3.2", got.KPI.CPUSavedCores)
	}
	if got.KPI.CPUSavedRatio != 0.18 {
		t.Errorf("kpi.cpuSavedRatio = %v, want 0.18", got.KPI.CPUSavedRatio)
	}
	if got.KPI.MemSavedBytes != 4096 {
		t.Errorf("kpi.memSavedBytes = %v, want 4096", got.KPI.MemSavedBytes)
	}
	if got.KPI.MemSavedRatio != 0.25 {
		t.Errorf("kpi.memSavedRatio = %v, want 0.25", got.KPI.MemSavedRatio)
	}
}

func TestHandleSummaryHeadroomAttentionPolicies(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{},
		byLabel: map[string]map[string]float64{
			"k8s_sustain:cluster_cpu_headroom_breakdown":    {"used": 0.4, "idle": 0.3, "free": 0.3},
			"k8s_sustain:cluster_memory_headroom_breakdown": {"used": 0.5, "idle": 0.2, "free": 0.3},
			"k8s_sustain:workload_oom_24h > 0":              {"checkout": 3, "api": 1},
			"k8s_sustain:workload_drifted == 1":             {"web": 1},
			"k8s_sustain_workload_retry_state == 1":         {"worker": 1},
			"k8s_sustain_policy_workload_count":             {"prod-policy": 7},
			"k8s_sustain:policy_cpu_savings_cores":          {"prod-policy": 1.5},
			"k8s_sustain:policy_memory_savings_bytes":       {"prod-policy": 2048},
			"k8s_sustain_policy_at_risk_count":              {"prod-policy": 2},
		},
	}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Logger:     testr.New(t),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got summaryResponseV2
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v body=%s", err, rec.Body.String())
	}

	cpuHR := got.Headroom["cpu"]
	if cpuHR.Used != 0.4 || cpuHR.Idle != 0.3 || cpuHR.Free != 0.3 {
		t.Errorf("headroom.cpu = %+v, want used=0.4 idle=0.3 free=0.3", cpuHR)
	}
	memHR := got.Headroom["memory"]
	if memHR.Used != 0.5 || memHR.Idle != 0.2 || memHR.Free != 0.3 {
		t.Errorf("headroom.memory = %+v, want used=0.5 idle=0.2 free=0.3", memHR)
	}

	risk := got.Attention["risk"]
	if len(risk) == 0 {
		t.Fatalf("expected attention.risk length > 0")
	}
	if risk[0].Signal != "OOM" {
		t.Errorf("attention.risk[0].Signal = %q, want OOM", risk[0].Signal)
	}
	// Deterministic order: highest value first ("checkout" = 3).
	if risk[0].Name != "checkout" {
		t.Errorf("attention.risk[0].Name = %q, want checkout (highest value)", risk[0].Name)
	}

	if len(got.Policies) != 1 || got.Policies[0].Name != "prod-policy" {
		t.Fatalf("policies = %+v, want one entry named prod-policy", got.Policies)
	}
	pol := got.Policies[0]
	if pol.WorkloadCount != 7 {
		t.Errorf("policies[0].WorkloadCount = %d, want 7", pol.WorkloadCount)
	}
	if pol.CPUSavingsCores != 1.5 {
		t.Errorf("policies[0].CPUSavingsCores = %v, want 1.5", pol.CPUSavingsCores)
	}
	if pol.MemSavingsBytes != 2048 {
		t.Errorf("policies[0].MemSavingsBytes = %v, want 2048", pol.MemSavingsBytes)
	}
	if pol.AtRiskCount != 2 {
		t.Errorf("policies[0].AtRiskCount = %d, want 2", pol.AtRiskCount)
	}
}

func TestHandleSummaryCacheHit(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{
			"k8s_sustain:cluster_cpu_savings_cores": 3.2,
		},
	}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Logger:     testr.New(t),
	}
	handler := srv.Handler()

	// First call: populates cache with 3.2.
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}
	var got1 summaryResponseV2
	if err := json.Unmarshal(rec1.Body.Bytes(), &got1); err != nil {
		t.Fatalf("decode 1: %v", err)
	}

	// Mutate the underlying client; cache should still serve old value.
	fp.instant["k8s_sustain:cluster_cpu_savings_cores"] = 99.9

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rec2.Code)
	}
	var got2 summaryResponseV2
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if got2.KPI.CPUSavedCores != got1.KPI.CPUSavedCores {
		t.Errorf("expected cached value %v, got %v", got1.KPI.CPUSavedCores, got2.KPI.CPUSavedCores)
	}

	// Reset cache and confirm the new value flows through.
	srv.summaryCache = NewCache(8, 60*time.Second)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("third call: expected 200, got %d", rec3.Code)
	}
	var got3 summaryResponseV2
	if err := json.Unmarshal(rec3.Body.Bytes(), &got3); err != nil {
		t.Fatalf("decode 3: %v", err)
	}
	if got3.KPI.CPUSavedCores != 99.9 {
		t.Errorf("after cache reset: expected 99.9, got %v", got3.KPI.CPUSavedCores)
	}
}

func TestHandleSummaryDoesNotCacheOnPromError(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{
			"k8s_sustain:cluster_cpu_savings_cores": 3.2,
		},
		instantErr: map[string]error{
			"k8s_sustain:cluster_memory_savings_bytes": errors.New("prom unreachable"),
		},
	}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Logger:     testr.New(t),
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (graceful degrade), got %d", rec.Code)
	}

	if _, ok := srv.summaryCache.Get("summary"); ok {
		t.Fatalf("cache was poisoned despite prom error")
	}
}
