package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

func TestHandleWorkloadMetrics_BadWindow(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics?window=bogus", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadMetrics_BadStep(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics?step=999", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadMetrics_ReturnsAllKeys(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "wait-db"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Inputs: recommendertest.NewStaticInputs(), Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	decodeEnvelopeData(t, rec.Body, &got)
	for _, k := range []string{"cpu", "memory", "resources", "cpuRequests", "memoryRequests", "oomEvents", "initContainers"} {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing %q key; got keys = %v", k, mapKeys(got))
		}
	}
	inits, _ := got["initContainers"].([]any)
	if len(inits) != 1 || inits[0] != "wait-db" {
		t.Errorf("initContainers = %v, want [wait-db]", inits)
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWorkloadMetricsAbsoluteRange(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	prom := &fakePromClient{}
	srv := &Server{K8sClient: c, PromClient: prom, Inputs: recommendertest.NewStaticInputs(), Logger: testLogger(t)}

	req := httptest.NewRequest(http.MethodGet,
		"/api/workloads/default/Deployment/web/metrics?from=1718000000&to=1718003600&step=5m", nil)
	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec, req, "default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	gotCPU := prom.capturedCPURange
	if gotCPU.Start.Unix() != 1718000000 || gotCPU.End.Unix() != 1718003600 {
		t.Errorf("range = [%d,%d], want [1718000000,1718003600]", gotCPU.Start.Unix(), gotCPU.End.Unix())
	}
}
