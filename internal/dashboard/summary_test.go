package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/noony/k8s-sustain/internal/governance"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// testLogger returns a logr.Logger that writes to the test's log.
func testLogger(t *testing.T) logr.Logger { return testr.New(t) }

// fakePromClient satisfies PromQuerier with canned values keyed by expression.
type fakePromClient struct {
	instant         map[string]float64
	byLabel         map[string]map[string]float64
	rangeByLabel    map[string]map[string][]promclient.TimeValue
	instantErr      map[string]error
	byLabelErr      map[string]error
	rangeByLabelErr map[string]error

	// mu guards the captured* fields; handleSummary queries from several
	// goroutines at once.
	mu             sync.Mutex
	capturedRange  promclient.TimeRange
	capturedRanges []promclient.TimeRange
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

func (f *fakePromClient) QueryRange(_ context.Context, _ string, tr promclient.TimeRange, _ time.Duration) ([]promclient.TimeValue, error) {
	f.mu.Lock()
	f.capturedRange = tr
	f.mu.Unlock()
	return nil, nil
}

func (f *fakePromClient) Ping(_ context.Context) error { return nil }

func (f *fakePromClient) QueryRangeByLabel(_ context.Context, expr, _ string, r promclient.TimeRange, _ time.Duration) (map[string][]promclient.TimeValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capturedRanges = append(f.capturedRanges, r)
	if err, ok := f.rangeByLabelErr[expr]; ok {
		return nil, err
	}
	return f.rangeByLabel[expr], nil
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
		Health:     memHealthSignals{},
		Logger:     testr.New(t),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)

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

func TestHandleSummaryHeadroomAndPolicies(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{},
		byLabel: map[string]map[string]float64{
			"k8s_sustain:cluster_cpu_headroom_breakdown":    {"used": 0.4, "idle": 0.3, "free": 0.3},
			"k8s_sustain:cluster_memory_headroom_breakdown": {"used": 0.5, "idle": 0.2, "free": 0.3},
			"k8s_sustain_policy_workload_count":             {"prod-policy": 7},
			"k8s_sustain:policy_cpu_savings_cores":          {"prod-policy": 1.5},
			"k8s_sustain:policy_memory_savings_bytes":       {"prod-policy": 2048},
		},
	}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Health:     memHealthSignals{},
		Logger:     testr.New(t),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got summaryResponseV2
	decodeEnvelopeData(t, rec.Body, &got)

	cpuHR := got.Headroom["cpu"]
	if cpuHR.Used != 0.4 || cpuHR.Idle != 0.3 || cpuHR.Free != 0.3 {
		t.Errorf("headroom.cpu = %+v, want used=0.4 idle=0.3 free=0.3", cpuHR)
	}
	memHR := got.Headroom["memory"]
	if memHR.Used != 0.5 || memHR.Idle != 0.2 || memHR.Free != 0.3 {
		t.Errorf("headroom.memory = %+v, want used=0.5 idle=0.2 free=0.3", memHR)
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
}

func TestHandleSummaryCacheHit(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{
			"k8s_sustain:cluster_cpu_savings_cores": 3.2,
		},
	}
	clk := clocktesting.NewFakePassiveClock(time.Now())
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Health:     memHealthSignals{},
		Clock:      clk,
		Policies:   governance.NewPolicyLister(nil),
		Logger:     testr.New(t),
	}
	handler := srv.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}
	var got1 summaryResponseV2
	decodeEnvelopeData(t, rec1.Body, &got1)

	fp.instant["k8s_sustain:cluster_cpu_savings_cores"] = 99.9

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rec2.Code)
	}
	var got2 summaryResponseV2
	decodeEnvelopeData(t, rec2.Body, &got2)
	if got2.KPI.CPUSavedCores != got1.KPI.CPUSavedCores {
		t.Errorf("expected cached value %v, got %v", got1.KPI.CPUSavedCores, got2.KPI.CPUSavedCores)
	}

	clk.SetTime(clk.Now().Add(61 * time.Second))
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("third call: expected 200, got %d", rec3.Code)
	}
	var got3 summaryResponseV2
	decodeEnvelopeData(t, rec3.Body, &got3)
	if got3.KPI.CPUSavedCores != 99.9 {
		t.Errorf("past the cache TTL: expected 99.9, got %v", got3.KPI.CPUSavedCores)
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
		Health:     memHealthSignals{},
		Logger:     testr.New(t),
	}

	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (graceful degrade), got %d", rec.Code)
	}

	fp.instant["k8s_sustain:cluster_cpu_savings_cores"] = 99.9
	fp.instantErr = nil
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	var got summaryResponseV2
	decodeEnvelopeData(t, rec2.Body, &got)
	if got.KPI.CPUSavedCores != 99.9 {
		t.Fatalf("expected a fresh recompute 99.9 after the partial one, got %v: the cache was poisoned despite prom error", got.KPI.CPUSavedCores)
	}
}

func TestHandleSummaryServesRecentLastGoodOnPartialError(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{"k8s_sustain:cluster_cpu_savings_cores": 3.2},
	}
	clk := clocktesting.NewFakePassiveClock(time.Now())
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Health:     memHealthSignals{},
		Clock:      clk,
		Policies:   governance.NewPolicyLister(nil),
		Logger:     testr.New(t),
	}
	handler := srv.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	clk.SetTime(clk.Now().Add(5 * time.Minute))
	fp.instant["k8s_sustain:cluster_cpu_savings_cores"] = 99.9
	fp.instantErr = map[string]error{
		"k8s_sustain:cluster_memory_savings_bytes": errors.New("prom unreachable"),
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("blip: expected 200, got %d", rec2.Code)
	}
	var got summaryResponseV2
	decodeEnvelopeData(t, rec2.Body, &got)
	if got.KPI.CPUSavedCores != 3.2 {
		t.Errorf("expected last-good 3.2, got %v", got.KPI.CPUSavedCores)
	}
}

func TestHandleSummaryServesFreshPartialWhenLastGoodTooOld(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{"k8s_sustain:cluster_cpu_savings_cores": 3.2},
	}
	clk := clocktesting.NewFakePassiveClock(time.Now())
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: fp,
		Health:     memHealthSignals{},
		Clock:      clk,
		Policies:   governance.NewPolicyLister(nil),
		Logger:     testr.New(t),
	}
	handler := srv.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	clk.SetTime(clk.Now().Add(maxSummaryLastGoodAge + time.Minute))
	fp.instant["k8s_sustain:cluster_cpu_savings_cores"] = 99.9
	fp.instantErr = map[string]error{
		"k8s_sustain:cluster_memory_savings_bytes": errors.New("prom unreachable"),
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("outage: expected 200, got %d", rec2.Code)
	}
	var got summaryResponseV2
	decodeEnvelopeData(t, rec2.Body, &got)
	if got.KPI.CPUSavedCores != 99.9 {
		t.Errorf("expected fresh-but-partial 99.9, got %v", got.KPI.CPUSavedCores)
	}
}

// blockingPromClient holds a /api/summary recompute open until release is
// closed.
type blockingPromClient struct {
	*fakePromClient

	release     <-chan struct{}
	started     chan struct{}
	startedOnce sync.Once
}

func (b *blockingPromClient) QueryInstant(ctx context.Context, expr string) (float64, error) {
	b.startedOnce.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		// Never hang the whole package if a barrier is missed.
	}
	return b.fakePromClient.QueryInstant(ctx, expr)
}

// A request that gives up on the recompute it joined, with no last-good
// snapshot to fall back on, answers 503 without the payload's max-age.
func TestHandleSummaryRequestGivingUpWithoutLastGoodIs503(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{promclient.MetricClusterCPUSavingsCores: 3.2},
	}
	leaderCanProceed := make(chan struct{})
	bp := &blockingPromClient{
		fakePromClient: fp,
		release:        leaderCanProceed,
		started:        make(chan struct{}),
	}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: bp,
		Health:     memHealthSignals{},
		Logger:     testr.New(t),
	}
	handler := srv.Handler()

	// Release the leader even on an early t.Fatal, or a goroutine still in the
	// handler leaks into the next shuffled test and trips goleak.
	leaderDone := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(leaderCanProceed) })
		select {
		case <-leaderDone:
		case <-time.After(5 * time.Second):
			t.Error("leader request still running after the test finished")
		}
	}()
	go func() {
		defer close(leaderDone)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	}()

	select {
	case <-bp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's recompute never started")
	}

	followerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil).WithContext(followerCtx))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("follower status = %d, want 503 (no last-good snapshot to fall back on)", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q on the 503, want it cleared", cc)
	}
}
