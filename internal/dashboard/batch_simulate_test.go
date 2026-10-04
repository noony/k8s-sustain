package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// newBatchSimulateServer builds a server with `count` Deployments, all annotated
// for policy "p", which opts the Deployment kind in. Each Deployment has one
// container named "main".
func newBatchSimulateServer(t *testing.T, inputs recommender.InputsFetcher, count int) *Server {
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
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Inputs: inputs}
}

func batchID(name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: name}
}

// Every workload of the policy is fetched in a single batch call, sized by
// its container count, and assembled without an error entry.
func TestBatchSimulateFetchesEveryWorkloadInOneCall(t *testing.T) {
	const total = 3
	inputs := recommendertest.NewStaticInputs()
	srv := newBatchSimulateServer(t, inputs, total)

	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")

	calls := inputs.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d fetch calls, want 1 batch for the whole policy", len(calls))
	}
	if len(calls[0]) != total {
		t.Fatalf("batch requested %d identities, want %d", len(calls[0]), total)
	}
	for _, r := range calls[0] {
		if r.Containers != 1 {
			t.Errorf("%v requested with size %d, want 1 (its container count)", r.Identity, r.Containers)
		}
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

// A workload whose inputs could not be fetched reports the error in its own
// entry; the others still get their recommendations.
func TestBatchSimulateReportsFetchFailurePerWorkload(t *testing.T) {
	inputs := recommendertest.NewStaticInputs().
		Fail(batchID("wl-a"), errors.New("prometheus down")).
		Set(batchID("wl-b"), &recommender.WorkloadInputs{CPUPerPod: promclient.ContainerValues{"main": 0.2}})
	srv := newBatchSimulateServer(t, inputs, 2)

	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")

	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	if len(resp.Workloads) != 2 {
		t.Fatalf("got %d workload results, want 2", len(resp.Workloads))
	}
	for _, w := range resp.Workloads {
		switch w.Name {
		case "wl-a":
			if !strings.Contains(w.Error, "prometheus down") {
				t.Errorf("wl-a error = %q, want the fetch failure", w.Error)
			}
		case "wl-b":
			if w.Error != "" || w.Containers["main"].RecommendedCPU != "200m" {
				t.Errorf("wl-b = %+v, want a 200m recommendation despite wl-a failing", w)
			}
		}
	}
}

// TestBatchSimulateAggregateSkipsRecsWithoutCurrentUsage pins the savings
// aggregate fix: a recommendation whose current usage was excluded (usage <= 0,
// e.g. an OOM-peak-only container) must not be added to the recommended total,
// otherwise SavingsPercent is deflated or flips negative.
func TestBatchSimulateAggregateSkipsRecsWithoutCurrentUsage(t *testing.T) {
	const mib = 1 << 20
	// wl-a has real usage; wl-b only has an OOM peak, so it gets a memory
	// recommendation but contributes nothing to the current total.
	inputs := recommendertest.NewStaticInputs().
		Set(batchID("wl-a"), &recommender.WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"main": 0.2},
			MemPerPod: promclient.ContainerValues{"main": 200 * mib},
		}).
		Set(batchID("wl-b"), &recommender.WorkloadInputs{
			OOM: promclient.OOMSignal{OOMCounts: promclient.ContainerValues{"main": 1}, PeakMemoryBytes: promclient.ContainerValues{"main": 300 * mib}},
		})
	srv := newBatchSimulateServer(t, inputs, 2)

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
