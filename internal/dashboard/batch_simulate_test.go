package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// newBatchSimulateServer builds a server with `count` Deployments, wl-a,
// wl-b, …, all annotated for policy "p", which opts the Deployment kind in.
// Each Deployment is two days old and has one container named "main"; the
// edit at its index, if any, changes it.
func newBatchSimulateServer(t *testing.T, inputs recommender.InputsFetcher, count int, edits ...func(*appsv1.Deployment)) *Server {
	t.Helper()
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode

	objs := []client.Object{policy}
	for i := range count {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "wl-" + string(rune('a'+i)),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		}}
		d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}
		if i < len(edits) {
			edits[i](d)
		}
		objs = append(objs, d)
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Inputs: inputs}
}

func batchID(name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: name}
}

// requesting runs container main with cpu and memory requests.
func requesting(cpu, memory string) func(*appsv1.Deployment) {
	return func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
	}
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
	if len(calls[0].Requests) != total {
		t.Fatalf("batch requested %d identities, want %d", len(calls[0].Requests), total)
	}
	for _, r := range calls[0].Requests {
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
		Set(batchID("wl-b"), recommender.WithCPUUsage(promclient.ContainerValues{"main": 0.2}))
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

// Savings compare the recommended requests with the requests the containers
// run with, not with their usage, which the recommendation is derived from. A
// container without a request has nothing to save on: it shows its
// recommendation but stays out of the totals.
func TestBatchSimulateSavingsAreAgainstTheCurrentRequests(t *testing.T) {
	inputs := recommendertest.NewStaticInputs().
		Set(batchID("wl-a"),
			recommender.WithCPUUsage(promclient.ContainerValues{"main": 0.2}),
			recommender.WithMemoryUsage(promclient.ContainerValues{"main": 64 << 20})).
		Set(batchID("wl-b"), recommender.WithCPUUsage(promclient.ContainerValues{"main": 0.3}))
	srv := newBatchSimulateServer(t, inputs, 2, requesting("500m", "256Mi"))

	rows, resp := batchSimulate(t, srv)

	want := batchContainerResult{
		CurrentCPU: "500m", RecommendedCPU: "200m", CPUDeltaPercent: -60,
		CurrentMemory: "256Mi", RecommendedMemory: "64Mi", MemDeltaPercent: -75,
	}
	if rows["wl-a"].Containers["main"] != want {
		t.Errorf("wl-a = %+v, want %+v", rows["wl-a"].Containers["main"], want)
	}
	if b := rows["wl-b"].Containers["main"]; b.CurrentCPU != "" || b.RecommendedCPU != "300m" {
		t.Errorf("wl-b = %+v, want its 300m recommendation and no current request", b)
	}
	if resp.CPU.CurrentMillis != 500 || resp.CPU.RecommendedMillis != 200 || resp.CPU.SavingsMillis != 300 {
		t.Errorf("cpu = %+v, want 500m → 200m: wl-a's request and recommendation only", resp.CPU)
	}
	if resp.Memory.SavingsPercent != 75 {
		t.Errorf("memory savings = %v%%, want 75%% (256Mi → 64Mi)", resp.Memory.SavingsPercent)
	}
}

// Each identity reports its outcome. A Too young identity still shows its
// numbers, as its Simulation does, but the controller would not apply them, so
// they stay out of the savings; a NoData one has none.
func TestBatchSimulateSavingsCountComputedIdentitiesOnly(t *testing.T) {
	usage := recommender.WithCPUUsage(promclient.ContainerValues{"main": 0.2})
	inputs := recommendertest.NewStaticInputs().Set(batchID("wl-a"), usage).Set(batchID("wl-b"), usage)
	young := func(d *appsv1.Deployment) {
		requesting("500m", "256Mi")(d)
		d.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	}
	srv := newBatchSimulateServer(t, inputs, 3, requesting("500m", "256Mi"), young, requesting("500m", "256Mi"))

	rows, resp := batchSimulate(t, srv)

	for name, want := range map[string]sustainv1alpha1.RecommendationOutcome{
		"wl-a": sustainv1alpha1.OutcomeComputed,
		"wl-b": sustainv1alpha1.OutcomeTooYoung,
		"wl-c": sustainv1alpha1.OutcomeNoData,
	} {
		if got := rows[name].Outcome; got != want {
			t.Errorf("%s outcome = %q, want %q", name, got, want)
		}
	}
	if got := rows["wl-b"].Containers["main"].RecommendedCPU; got != "200m" {
		t.Errorf("wl-b recommended %q, want its 200m shown", got)
	}
	if resp.CPU.CurrentMillis != 500 || resp.CPU.RecommendedMillis != 200 {
		t.Errorf("cpu = %+v, want wl-a's 500m → 200m alone", resp.CPU)
	}
}

// batchSimulate runs the policy p's batch simulation on srv and returns its
// rows by name.
func batchSimulate(t *testing.T, srv *Server) (map[string]workloadBatchResult, batchSimulateResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handlePolicyBatchSimulate(rec, httptest.NewRequest(http.MethodGet, "/api/policies/p/batch-simulate", nil), "p")
	var resp batchSimulateResponse
	decodeEnvelopeData(t, rec.Body, &resp)
	rows := map[string]workloadBatchResult{}
	for _, w := range resp.Workloads {
		rows[w.Name] = w
	}
	return rows, resp
}
