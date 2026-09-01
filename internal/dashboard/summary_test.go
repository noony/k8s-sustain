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

	// Per-container memory percentile bytes returned by QueryMemoryByContainer.
	memByContainer promclient.ContainerValues
	// OOM signal returned by QueryWorkloadOOMSignal (zero value = no OOM).
	oomSignal promclient.OOMSignal

	// mu guards the captured* fields, which handleSummary writes from
	// multiple goroutines concurrently (e.g. two sparkline QueryRange calls).
	mu sync.Mutex
	// capturedCPURange records the last TimeRange passed to QueryCPURangeByContainer.
	capturedCPURange promclient.TimeRange
	// capturedRange records the last TimeRange passed to QueryRange.
	capturedRange promclient.TimeRange
	// capturedRecRange records the last TimeRange passed to QueryCPURecommendationRangeByContainer.
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

func (f *fakePromClient) QueryCPUByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
	return promclient.ContainerValues{}, nil
}

func (f *fakePromClient) QueryMemoryByContainer(_ context.Context, _, _, _ string, _ float64, _ string) (promclient.ContainerValues, error) {
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

func (f *fakePromClient) QueryCPURecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, r promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	f.mu.Lock()
	f.capturedRecRange = r
	f.mu.Unlock()
	return promclient.ContainerTimeSeries{}, nil
}

func (f *fakePromClient) QueryMemoryRecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
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
		// Attention queries are keyed by the full namespace|kind|name triple so
		// rows carry the click-through identity.
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
	// Deterministic order: highest value first ("checkout" = 3). Namespace and
	// Kind must be populated so the UI can link to /workloads/{ns}/{kind}/{name}.
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

	// First call: populates cache with 3.2.
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}
	var got1 summaryResponseV2
	decodeEnvelopeData(t, rec1.Body, &got1)

	// Mutate the underlying client; cache should still serve old value.
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

	// Reset cache and confirm the new value flows through.
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

// On a brief Prometheus blip (partial failure, last-good still recent) the
// handler serves the most recent complete snapshot rather than the partial one.
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

	// Prime the cache with a complete snapshot (CPU=3.2).
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	// TTL lapses but we are still within maxSummaryLastGoodAge; a partial
	// failure should serve the stale-but-recent last-good value (3.2).
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

// During a sustained outage longer than maxSummaryLastGoodAge the handler stops
// serving the stale snapshot and falls back to the fresh-but-partial result.
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

	// Prime the cache with a complete snapshot (CPU=3.2).
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime: expected 200, got %d", rec1.Code)
	}

	// Advance past maxSummaryLastGoodAge and induce a partial failure. The
	// stale last-good is now too old, so the fresh partial value (99.9) wins.
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

// TestCollectPolicyWorkloads_Basic pins collectPolicyWorkloads' own contract
// directly rather than only through its one caller, handlePolicyBatchSimulate
// (batch_simulate.go) — it was otherwise untested in isolation. It does NOT
// back the summary page's per-policy rollups; those come from Prometheus via
// fetchPolicyRollups (handlers_policies.go).
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

// TestCollectPolicyWorkloads_SelectorExcludesNamespaceOptIn is the summary
// rollup's counterpart to TestPolicyWorkloads_NamespaceOptIn_SelectorExcludesIt:
// a Namespace naming a Policy whose own selector does not reach it must not
// inflate that policy's summary counts.
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

// blockingPromClient wraps fakePromClient so a test can hold a /api/summary
// recompute open (every QueryInstant blocks until release is closed) and
// count how many times each expression was actually queried. Counting per
// expression rather than in total keeps the assertions stable if the handler
// gains or loses a query later.
type blockingPromClient struct {
	*fakePromClient

	// release gates every QueryInstant; close it to let the recompute finish.
	release <-chan struct{}
	// started, when non-nil, is closed the first time a query runs, so a test
	// can wait for the leader to be genuinely in flight.
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
		// Safety valve: never hang the whole package if a barrier is missed.
	}
	return b.fakePromClient.QueryInstant(ctx, expr)
}

func (b *blockingPromClient) count(expr string) int {
	b.countMu.Lock()
	defer b.countMu.Unlock()
	return b.counts[expr]
}

// TestHandleSummaryConcurrentMissesCollapseToOneRecompute pins the fix for a
// check-then-act cache: Get-miss, fan ~16 Prometheus queries out, Set. Without
// deduplication N concurrent misses each fired their own full fan-out (16N
// round-trips), and — because the Set was unconditional — a slow one could
// land AFTER a faster, newer snapshot and overwrite it with older data while
// resetting the 60s TTL, so every client saw staler data than the cache had
// already held.
//
// Server.sfJoinHook holds the recompute open until all N requests have joined
// the same in-flight computation, so this genuinely exercises concurrency
// rather than passing on scheduler luck: without the singleflight every one
// of the N goroutines would run its own fan-out and this would report N
// queries per expression, not 1. Exactly one recompute also means exactly one
// cache Set, which is what makes the stale-overwrite unreachable.
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

	// Every waiter must see the one snapshot that was computed, and the cache
	// must hold that same snapshot — no later Set may have replaced it.
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

// TestHandleSummaryFollowerHonoursOwnContextDeadline: joining another
// request's in-flight recompute must not make a request wait past its OWN
// context deadline. The leader keeps running for whoever still has budget.
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

	// The leader must be JOINED before this test returns, not merely
	// released: a goroutine still inside the handler after its own test has
	// finished runs concurrently with whatever test -shuffle puts next, and
	// the package's goleak check would flag it. Releasing here rather than
	// only on the happy path keeps an early t.Fatal from stranding it.
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
	// The handler sets max-age=60 for the summary payload before it knows
	// whether it has one. An error must not go out carrying it, or a browser
	// or intermediary re-serves this 503 for a minute after recovery.
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q on the 503, want it cleared", cc)
	}
}

// TestComputeAndCacheSummaryRecoversPanic pins the singleflight leader's
// panic safety. A panic that escapes a singleflight function while DoChan
// waiters are parked on its channel is unrecoverable — it aborts the whole
// process rather than surfacing as a 500 on one request — so the leader must
// convert any panic on its own goroutine into an error, which every waiter
// then re-panics with under the normal HTTP recovery middleware.
//
// The nil summaryCache is the stand-in trigger: the leader's trailing Set
// dereferences it and panics.
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
