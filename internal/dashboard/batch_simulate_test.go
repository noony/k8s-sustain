package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// blockingBatchPromClient is a PromQuerier stub for the batch-simulate handler.
// QueryCPUByContainer signals its arrival (one signal per dispatched workload,
// keyed by owner name) and then blocks until release is closed or the context
// is cancelled, letting a test pin all bounded-spawn slots open while it cancels
// the request. Every other method returns zero values.
type blockingBatchPromClient struct {
	arrived chan string   // receives one owner name per dispatched workload
	release chan struct{} // closed by the test to unblock in-flight queries

	mu         sync.Mutex
	dispatched map[string]struct{} // distinct owner names that reached QueryCPUByContainer
}

func newBlockingBatchPromClient() *blockingBatchPromClient {
	return &blockingBatchPromClient{
		arrived:    make(chan string, 128),
		release:    make(chan struct{}),
		dispatched: make(map[string]struct{}),
	}
}

func (f *blockingBatchPromClient) QueryCPUByContainer(ctx context.Context, _, _, ownerName string, _ float64, _ string) (promclient.ContainerValues, error) {
	f.mu.Lock()
	f.dispatched[ownerName] = struct{}{}
	f.mu.Unlock()
	f.arrived <- ownerName
	select {
	case <-f.release:
		return promclient.ContainerValues{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *blockingBatchPromClient) distinctDispatched() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatched)
}

func (f *blockingBatchPromClient) wasDispatched(ownerName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.dispatched[ownerName]
	return ok
}

func (f *blockingBatchPromClient) Ping(context.Context) error { return nil }
func (f *blockingBatchPromClient) QueryInstant(context.Context, string) (float64, error) {
	return 0, nil
}

func (f *blockingBatchPromClient) QueryRange(_ context.Context, _ string, _ promclient.TimeRange, _ string) ([]promclient.TimeValue, error) {
	return nil, nil
}

func (f *blockingBatchPromClient) QueryByLabel(context.Context, string, string) (map[string]float64, error) {
	return map[string]float64{}, nil
}

func (f *blockingBatchPromClient) QueryByLabels(context.Context, string, ...string) (map[string]float64, error) {
	return map[string]float64{}, nil
}

func (f *blockingBatchPromClient) QueryMemoryByContainer(context.Context, string, string, string, float64, string) (promclient.ContainerValues, error) {
	return promclient.ContainerValues{}, nil
}

func (f *blockingBatchPromClient) QueryCPURangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryMemoryRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryCPURequestRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryMemoryRequestRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryCPULimitRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryMemoryLimitRangeByContainer(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryCPURecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryMemoryRecommendationRangeByContainer(_ context.Context, _, _, _ string, _ float64, _ string, _ promclient.TimeRange, _ string) (promclient.ContainerTimeSeries, error) {
	return promclient.ContainerTimeSeries{}, nil
}

func (f *blockingBatchPromClient) QueryOOMKillEvents(_ context.Context, _, _, _ string, _ promclient.TimeRange, _ string) ([]promclient.OOMEvent, error) {
	return nil, nil
}

func (f *blockingBatchPromClient) QueryWorkloadOOMSignal(context.Context, string, string, string) (promclient.OOMSignal, error) {
	return promclient.OOMSignal{}, nil
}

// newBatchSimulateServer builds a server with `count` Deployments, all annotated
// for policy "p", which opts the Deployment kind in. Each Deployment has one
// container so the batch handler dispatches exactly one CPU query per workload.
func newBatchSimulateServer(t *testing.T, prom PromQuerier, count int) *Server {
	t.Helper()
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode

	objs := []client.Object{policy}
	for i := range count {
		name := "wl-" + string(rune('a'+i))
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		}
		d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}
		objs = append(objs, d)
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: prom}
}

// TestBatchSimulateStopsDispatchOnCancel verifies that once the request context
// is cancelled while the bounded-spawn semaphore is saturated, the handler stops
// dispatching new workloads. With 12 matching workloads and a 10-slot semaphore,
// exactly the first 10 reach Prometheus; the remaining 2 must surface a
// context-cancellation error instead of being dispatched after slots free up.
func TestBatchSimulateStopsDispatchOnCancel(t *testing.T) {
	const total = 12
	const slots = 10

	prom := newBlockingBatchPromClient()
	srv := newBatchSimulateServer(t, prom, total)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handlePolicyBatchSimulate(rec, req, "p")
		close(done)
	}()

	// Wait until all 10 semaphore slots are pinned by in-flight CPU queries.
	for range slots {
		<-prom.arrived
	}
	// Cancel while the semaphore is provably full: the spawn loop is now blocked
	// trying to start workload #11, so the post-fix select cannot race.
	cancel()
	// Release the blocked in-flight queries; pre-fix code drains the freed slots
	// by dispatching the remaining workloads despite the dead context.
	close(prom.release)
	<-done

	if got := prom.distinctDispatched(); got != slots {
		t.Fatalf("dispatched %d workloads, want %d (cancellation must stop further dispatch)", got, slots)
	}

	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Workloads) != total {
		t.Fatalf("got %d workload results, want %d", len(resp.Workloads), total)
	}

	// Every undispatched workload (one the stub never observed) must carry a
	// context-cancellation error so the UI reports it rather than silently
	// dropping it. Dispatched workloads ran with a dead context and may fail
	// fast too, which is acceptable, so they are not asserted on here.
	var undispatched int
	for _, w := range resp.Workloads {
		if prom.wasDispatched(w.Name) {
			continue
		}
		undispatched++
		if !strings.Contains(w.Error, context.Canceled.Error()) {
			t.Fatalf("undispatched workload %s has error %q, want context-cancellation", w.Name, w.Error)
		}
	}
	if undispatched != total-slots {
		t.Fatalf("got %d undispatched workloads, want %d", undispatched, total-slots)
	}
}

// TestBatchSimulateDispatchesAllWhenUnderCapacity is the happy-path regression
// guard: with fewer workloads than semaphore slots and no cancellation, every
// workload is dispatched and assembled without an error entry.
func TestBatchSimulateDispatchesAllWhenUnderCapacity(t *testing.T) {
	const total = 3

	prom := newBlockingBatchPromClient()
	close(prom.release) // never block
	srv := newBatchSimulateServer(t, prom, total)

	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")

	if got := prom.distinctDispatched(); got != total {
		t.Fatalf("dispatched %d workloads, want %d", got, total)
	}
	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Workloads) != total {
		t.Fatalf("got %d workload results, want %d", len(resp.Workloads), total)
	}
	for _, w := range resp.Workloads {
		if w.Error != "" {
			t.Fatalf("workload %s/%s carried unexpected error: %s", w.Namespace, w.Name, w.Error)
		}
	}
}

// usageBatchPromClient returns per-owner canned usage and OOM signals so the
// aggregate-savings test can mix containers with and without current usage.
type usageBatchPromClient struct {
	fakePromClient
	cpuByOwner map[string]promclient.ContainerValues
	memByOwner map[string]promclient.ContainerValues
	oomByOwner map[string]promclient.OOMSignal
}

func (f *usageBatchPromClient) QueryCPUByContainer(_ context.Context, _, _, ownerName string, _ float64, _ string) (promclient.ContainerValues, error) {
	return f.cpuByOwner[ownerName], nil
}

func (f *usageBatchPromClient) QueryMemoryByContainer(_ context.Context, _, _, ownerName string, _ float64, _ string) (promclient.ContainerValues, error) {
	return f.memByOwner[ownerName], nil
}

func (f *usageBatchPromClient) QueryWorkloadOOMSignal(_ context.Context, _, _, ownerName string) (promclient.OOMSignal, error) {
	return f.oomByOwner[ownerName], nil
}

// TestBatchSimulateAggregateSkipsRecsWithoutCurrentUsage pins the savings
// aggregate fix: a recommendation whose current usage was excluded (usage <= 0,
// e.g. an OOM-peak-only container) must not be added to the recommended total,
// otherwise SavingsPercent is deflated or flips negative.
func TestBatchSimulateAggregateSkipsRecsWithoutCurrentUsage(t *testing.T) {
	const mib = 1 << 20
	prom := &usageBatchPromClient{
		// wl-a has real usage; wl-b only has an OOM peak, so it gets a memory
		// recommendation but contributes nothing to the current total.
		cpuByOwner: map[string]promclient.ContainerValues{"wl-a": {"main": 0.2}},
		memByOwner: map[string]promclient.ContainerValues{"wl-a": {"main": 200 * mib}},
		oomByOwner: map[string]promclient.OOMSignal{
			"wl-b": {OOMCounts: promclient.ContainerValues{"main": 1}, PeakMemoryBytes: promclient.ContainerValues{"main": 300 * mib}},
		},
	}
	srv := newBatchSimulateServer(t, prom, 2)

	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)

	// Recompute the expected aggregate from the per-container rows: only
	// recommendations whose current value was counted may contribute.
	var wantMemRec int64
	var sawExcludedRec bool
	for _, wl := range resp.Workloads {
		for _, c := range wl.Containers {
			if c.RecommendedMemory == "" {
				continue
			}
			if c.CurrentMemory == "" {
				sawExcludedRec = true
				continue
			}
			q, err := resource.ParseQuantity(c.RecommendedMemory)
			if err != nil {
				t.Fatalf("parse recommended memory %q: %v", c.RecommendedMemory, err)
			}
			wantMemRec += q.MilliValue()
		}
	}
	if !sawExcludedRec {
		t.Fatal("fixture broken: expected a recommendation without current usage (wl-b)")
	}
	if resp.Memory.RecommendedMillis != wantMemRec {
		t.Errorf("memory.recommendedMillis = %d, want %d (recs without counted usage must be excluded)",
			resp.Memory.RecommendedMillis, wantMemRec)
	}
}
