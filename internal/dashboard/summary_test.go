package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// testLogger returns a logr.Logger that writes to the test's log.
func testLogger(t *testing.T) logr.Logger { return testr.New(t) }

// fakePromClient satisfies PromQuerier with canned values keyed by expression.
type fakePromClient struct {
	instant    map[string]float64
	byLabel    map[string]map[string]float64
	byLabels   map[string]map[string]float64
	instantErr map[string]error
	byLabelErr map[string]error

	memByContainer promclient.ContainerValues
	oomSignal      promclient.OOMSignal

	// mu guards the captured* fields; handleSummary queries from several
	// goroutines at once.
	mu               sync.Mutex
	capturedCPURange promclient.TimeRange
	capturedRange    promclient.TimeRange
	capturedRecRange promclient.TimeRange
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

func (f *fakePromClient) QueryRange(_ context.Context, _ string, tr promclient.TimeRange, _ string) ([]promclient.TimeValue, error) {
	f.mu.Lock()
	f.capturedRange = tr
	f.mu.Unlock()
	return nil, nil
}

func (f *fakePromClient) Ping(_ context.Context) error { return nil }

func (f *fakePromClient) QueryWorkloadCPUByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
	return promclient.ContainerValues{}, nil
}

func (f *fakePromClient) QueryWorkloadMemoryByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
	if f.memByContainer != nil {
		return f.memByContainer, nil
	}
	return promclient.ContainerValues{}, nil
}

func (f *fakePromClient) QueryCPURangeByContainer(_ context.Context, _, _, _ string, r promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	f.mu.Lock()
	f.capturedCPURange = r
	f.mu.Unlock()
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryCPURequestRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRequestRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryCPULimitRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryLimitRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryWorkloadCPURecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, r promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	f.mu.Lock()
	f.capturedRecRange = r
	f.mu.Unlock()
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryWorkloadMemoryRecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryOOMKillEvents(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) ([]promclient.OOMEvent, error) {
	return nil, nil
}

func (f *fakePromClient) QueryWorkloadOOMSignal(_ context.Context, _, _, _ string) (promclient.OOMSignal, error) {
	return f.oomSignal, nil
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

func TestHandleSummaryHeadroomAttentionPolicies(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{},
		byLabel: map[string]map[string]float64{
			"k8s_sustain:cluster_cpu_headroom_breakdown":    {"used": 0.4, "idle": 0.3, "free": 0.3},
			"k8s_sustain:cluster_memory_headroom_breakdown": {"used": 0.5, "idle": 0.2, "free": 0.3},
			"k8s_sustain_policy_workload_count":             {"prod-policy": 7},
			"k8s_sustain:policy_cpu_savings_cores":          {"prod-policy": 1.5},
			"k8s_sustain:policy_memory_savings_bytes":       {"prod-policy": 2048},
			"k8s_sustain_policy_at_risk_count":              {"prod-policy": 2},
		},
		byLabels: map[string]map[string]float64{
			"sum by (namespace, owner_kind, owner_name) (k8s_sustain:workload_oom_24h) > 0": {"shop|Deployment|checkout": 3, "prod|StatefulSet|api": 1},
			"k8s_sustain:workload_drifted == 1":                                             {"prod|Deployment|web": 1},
			"k8s_sustain_workload_retry_state == 1":                                         {"prod|Deployment|worker": 1},
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
	decodeEnvelopeData(t, rec.Body, &got)

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
	if risk[0].Namespace != "shop" || risk[0].Kind != "Deployment" || risk[0].Name != "checkout" {
		t.Errorf("attention.risk[0] = %+v, want shop/Deployment/checkout (highest value)", risk[0])
	}
	if risk[1].Namespace != "prod" || risk[1].Kind != "StatefulSet" || risk[1].Name != "api" {
		t.Errorf("attention.risk[1] = %+v, want prod/StatefulSet/api", risk[1])
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

	srv.summaryCache = NewCache(8, 60*time.Second)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("third call: expected 200, got %d", rec3.Code)
	}
	var got3 summaryResponseV2
	decodeEnvelopeData(t, rec3.Body, &got3)
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

func TestHandleSummaryServesRecentLastGoodOnPartialError(t *testing.T) {
	fp := &fakePromClient{
		instant: map[string]float64{"k8s_sustain:cluster_cpu_savings_cores": 3.2},
	}
	now := time.Now()
	cache := NewCache(8, 60*time.Second)
	cache.now = func() time.Time { return now }
	srv := &Server{
		K8sClient:    fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient:   fp,
		Logger:       testr.New(t),
		summaryCache: cache,
	}
	handler := srv.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	now = now.Add(5 * time.Minute)
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
	now := time.Now()
	cache := NewCache(8, 60*time.Second)
	cache.now = func() time.Time { return now }
	srv := &Server{
		K8sClient:    fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient:   fp,
		Logger:       testr.New(t),
		summaryCache: cache,
	}
	handler := srv.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	now = now.Add(maxSummaryLastGoodAge + time.Minute)
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

// collectPolicyWorkloads feeds handlePolicyBatchSimulate, not the summary
// rollups; this tests it in isolation.
func TestCollectPolicyWorkloads_Basic(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "unmanaged"}}

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, d, other).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	got := srv.collectPolicyWorkloads(context.Background(), "p", policy)
	if len(got) != 1 || got[0].Name != "web" || got[0].Kind != "Deployment" || got[0].PolicyName != "p" {
		t.Fatalf("collectPolicyWorkloads = %+v, want exactly the pod-template-annotated deployment", got)
	}
}

func TestCollectPolicyWorkloads_SelectorExcludesNamespaceOptIn(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	policy.Spec.Selector.Namespaces = []string{"other-namespace"}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, ns, d).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	got := srv.collectPolicyWorkloads(context.Background(), "p", policy)
	if len(got) != 0 {
		t.Fatalf("a namespace must not opt into a policy whose selector excludes it, got %+v", got)
	}
}

// blockingPromClient holds a /api/summary recompute open until release is
// closed and counts queries per expression.
type blockingPromClient struct {
	*fakePromClient

	release     <-chan struct{}
	started     chan struct{}
	startedOnce sync.Once

	countMu sync.Mutex
	counts  map[string]int
}

func (b *blockingPromClient) QueryInstant(ctx context.Context, expr string) (float64, error) {
	b.countMu.Lock()
	if b.counts == nil {
		b.counts = map[string]int{}
	}
	b.counts[expr]++
	b.countMu.Unlock()
	if b.started != nil {
		b.startedOnce.Do(func() { close(b.started) })
	}
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		// Never hang the whole package if a barrier is missed.
	}
	return b.fakePromClient.QueryInstant(ctx, expr)
}

func (b *blockingPromClient) count(expr string) int {
	b.countMu.Lock()
	defer b.countMu.Unlock()
	return b.counts[expr]
}

// N concurrent cache misses must collapse into one recompute and one cache
// Set; sfJoinHook holds the recompute open until all N requests have joined.
func TestHandleSummaryConcurrentMissesCollapseToOneRecompute(t *testing.T) {
	const n = 8
	fp := &fakePromClient{
		instant: map[string]float64{promclient.MetricClusterCPUSavingsCores: 3.2},
	}

	var joined int32
	allJoined := make(chan struct{})
	bp := &blockingPromClient{fakePromClient: fp, release: allJoined}
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: bp,
		Logger:     testr.New(t),
		sfJoinHook: func(string) {
			if atomic.AddInt32(&joined, 1) == n {
				close(allJoined)
			}
		},
	}
	handler := srv.Handler()

	var wg sync.WaitGroup
	bodies := make([]string, n)
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	if got := bp.count(promclient.MetricClusterCPUSavingsCores); got != 1 {
		t.Errorf("expected exactly 1 Prometheus query for %d concurrent cache misses, got %d", n, got)
	}
	for i := range n {
		if codes[i] != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, codes[i])
		}
	}

	var want summaryResponseV2
	decodeEnvelopeData(t, bytes.NewReader([]byte(bodies[0])), &want)
	if want.KPI.CPUSavedCores != 3.2 {
		t.Fatalf("CPUSavedCores = %v, want 3.2", want.KPI.CPUSavedCores)
	}
	for i := 1; i < n; i++ {
		var got summaryResponseV2
		decodeEnvelopeData(t, bytes.NewReader([]byte(bodies[i])), &got)
		if got.KPI.CPUSavedCores != want.KPI.CPUSavedCores {
			t.Errorf("request %d saw CPUSavedCores %v, want the single shared snapshot %v",
				i, got.KPI.CPUSavedCores, want.KPI.CPUSavedCores)
		}
	}
	cached, ok := srv.summaryCache.Get(summaryCacheKey)
	if !ok {
		t.Fatal("cache was not populated by the shared recompute")
	}
	if got := cached.(summaryResponseV2).KPI.CPUSavedCores; got != want.KPI.CPUSavedCores {
		t.Errorf("cached CPUSavedCores = %v, want %v (a second Set overwrote the shared snapshot)", got, want.KPI.CPUSavedCores)
	}
}

// A follower must not wait past its own context deadline for the leader.
func TestHandleSummaryFollowerHonoursOwnContextDeadline(t *testing.T) {
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
	start := time.Now()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil).WithContext(followerCtx))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("follower blocked for %v past its own 20ms deadline; a slow leader must not borrow another caller's budget", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("follower status = %d, want 503 (no last-good snapshot to fall back on)", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q on the 503, want it cleared", cc)
	}
}

// A panic escaping a singleflight function with parked DoChan waiters aborts
// the process, so the leader must convert it to an error. The nil
// summaryCache is the trigger: the trailing Set dereferences it.
func TestComputeAndCacheSummaryRecoversPanic(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Logger:     testr.New(t),
	}

	val, err := srv.computeAndCacheSummary()
	if err == nil {
		t.Fatal("expected a recovered panic to surface as an error, got nil")
	}
	if !strings.Contains(err.Error(), "panic recomputing summary") {
		t.Errorf("error = %q, want it to name the recovered panic", err)
	}
	if val != nil {
		t.Errorf("val = %#v, want nil alongside the error", val)
	}
}
