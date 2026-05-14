package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// ---- handleWorkloadRecommendations ----

func TestHandleWorkloadRecommendations_WorkloadNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/missing/recommendations", nil),
		"default", "Deployment", "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadRecommendations_UnmanagedWorkload(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	// Pod template has no policy annotation → unmanaged.
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	if got.Automated {
		t.Errorf("Automated = true, want false for unannotated workload")
	}
	if got.PolicyName != "" {
		t.Errorf("PolicyName = %q, want empty", got.PolicyName)
	}
}

func TestHandleWorkloadRecommendations_PolicyMissing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "ghost"}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadRecommendations_BadWindow(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations?window=bogus", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadRecommendations_HappyPath(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	if !got.Automated {
		t.Errorf("Automated = false, want true")
	}
	if got.PolicyName != "p" {
		t.Errorf("PolicyName = %q, want %q", got.PolicyName, "p")
	}
}

// ---- handleWorkloadMetrics ----

func TestHandleWorkloadMetrics_BadWindow(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
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
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

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
