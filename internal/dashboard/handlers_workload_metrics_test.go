package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

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

// A workload naming a Policy that does not exist must agree with
// /api/workloads (Automated: false), not 404.
func TestHandleWorkloadRecommendations_PolicyMissing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "ghost"}
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
		t.Errorf("Automated = true, want false: workload opts into a Policy that does not exist")
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

// After a recent OOM the kernel peak must floor the recommendation, as the
// controller does.
func TestHandleWorkloadRecommendations_AppliesOOMFloor(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "stress"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).Build()

	const mib = 1 << 20
	prom := &fakePromClient{
		memByContainer: promclient.ContainerValues{"app": 100 * mib},
		oomSignal: promclient.OOMSignal{
			OOMCounts:       promclient.ContainerValues{"app": 1},
			PeakMemoryBytes: promclient.ContainerValues{"app": 200 * mib},
		},
	}
	srv := &Server{K8sClient: c, PromClient: prom, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/stress/recommendations", nil),
		"default", "Deployment", "stress")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	app, ok := got.Containers["app"]
	if !ok {
		t.Fatalf("missing app container in response: %+v", got.Containers)
	}
	if app.MemoryRequest != "200Mi" {
		t.Errorf("MemoryRequest = %q, want 200Mi (OOM floor)", app.MemoryRequest)
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

// Automated must reflect the Policy's selector, so this endpoint agrees with
// /api/workloads.
func TestHandleWorkloadRecommendations_SelectorExcludesWorkload(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "web",
		Labels:    map[string]string{"team": "a"},
	}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
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
	if got.Automated {
		t.Errorf("Automated = true, want false: the policy's LabelSelector excludes this workload's labels")
	}
	if got.PolicyName != "" {
		t.Errorf("PolicyName = %q, want empty", got.PolicyName)
	}
}

// A retained-WLR entry has no labels; the selector gate must skip the label
// half for it or departed workloads always fail it.
func TestHandleWorkloadRecommendations_DepartedWorkloadSkipsSelectorGate(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}}
	wlr := retainedWLR("p", "airflow", "Pod", "etl")
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, wlr).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/airflow/Pod/etl/recommendations", nil),
		"airflow", "Pod", "etl")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	if !got.Automated {
		t.Errorf("Automated = false, want true: a departed workload's WLR is the controller's own verdict that it matched")
	}
	if got.PolicyName != "p" {
		t.Errorf("PolicyName = %q, want %q", got.PolicyName, "p")
	}
}

// The namespace half of the gate still applies to a retained-WLR entry.
func TestHandleWorkloadRecommendations_DepartedWorkloadInExcludedNamespace(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	wlr := retainedWLR("p", "airflow", "Pod", "etl")
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, wlr).Build()
	srv := &Server{
		K8sClient:          c,
		PromClient:         &fakePromClient{},
		Logger:             testLogger(t),
		ExcludedNamespaces: []string{"airflow"},
	}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/airflow/Pod/etl/recommendations", nil),
		"airflow", "Pod", "etl")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	if got.Automated {
		t.Errorf("Automated = true, want false: entry.Namespace is excluded via --excluded-namespaces and must still be honoured for retained-WLR entries")
	}
}

// Both endpoints must report the same verdict when only a grouped sibling
// opts into and matches a Policy.
func TestHandleWorkloadRecommendations_GroupedIdentitySiblingOptsInAndMatches(t *testing.T) {
	p := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	p.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"track": "green"}}
	q := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "q"}}
	q.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"track": "nonexistent"}}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := deploymentWithOwnerNamePolicyAndLabels("team-a", "checkout-blue", "checkout", "q",
		map[string]string{"track": "blue"}, baseTime.Add(time.Hour))
	older := deploymentWithOwnerNamePolicyAndLabels("team-a", "checkout-green", "checkout", "p",
		map[string]string{"track": "green"}, baseTime)

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(p, q, older, newer).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/team-a/Deployment/checkout/recommendations", nil),
		"team-a", "Deployment", "checkout")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got recommendationResult
	decodeEnvelopeData(t, rec.Body, &got)
	if !got.Automated || got.PolicyName != "p" {
		t.Errorf("handleWorkloadRecommendations: Automated=%v PolicyName=%q, want Automated=true PolicyName=%q "+
			"(the sibling opts into p and satisfies p's own selector)", got.Automated, got.PolicyName, "p")
	}

	rec2 := httptest.NewRecorder()
	srv.handleAllWorkloads(rec2, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var listResp struct {
		Items []struct {
			Name       string `json:"name"`
			Automated  bool   `json:"automated"`
			PolicyName string `json:"policyName"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec2.Body, &listResp)
	var found bool
	for _, item := range listResp.Items {
		if item.Name != "checkout" {
			continue
		}
		found = true
		if !item.Automated || item.PolicyName != "p" {
			t.Errorf("/api/workloads: Automated=%v PolicyName=%q, want Automated=true PolicyName=%q",
				item.Automated, item.PolicyName, "p")
		}
	}
	if !found {
		t.Fatalf("expected a %q entry in /api/workloads, got %+v", "checkout", listResp.Items)
	}
}

// A failed Policy List must fail the request rather than report Automated:
// false.
func TestHandleWorkloadRecommendations_PoliciesListFails(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	// The Policy exists, so only the List path can fail here.
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*sustainv1alpha1.PolicyList); ok {
					return errors.New("apiserver is down")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	srv := &Server{K8sClient: c, PromClient: &fakePromClient{}, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetWorkloadPolicyAnnotation_Managed(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	got, err := srv.getWorkloadPolicyAnnotation(context.Background(), "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "p" {
		t.Errorf("policyName = %q, want %q", got, "p")
	}
}

func TestGetWorkloadPolicyAnnotation_Unmanaged(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	got, err := srv.getWorkloadPolicyAnnotation(context.Background(), "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("policyName = %q, want empty", got)
	}
}

// getWorkloadPolicyAnnotation must also search every member behind a grouped
// identity.
func TestGetWorkloadPolicyAnnotation_GroupedIdentitySiblingOptsInAndMatches(t *testing.T) {
	p := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	p.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"track": "green"}}
	q := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "q"}}
	q.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"track": "nonexistent"}}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := deploymentWithOwnerNamePolicyAndLabels("team-a", "checkout-blue", "checkout", "q",
		map[string]string{"track": "blue"}, baseTime.Add(time.Hour))
	older := deploymentWithOwnerNamePolicyAndLabels("team-a", "checkout-green", "checkout", "p",
		map[string]string{"track": "green"}, baseTime)

	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(p, q, older, newer).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	got, err := srv.getWorkloadPolicyAnnotation(context.Background(), "team-a", "Deployment", "checkout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "p" {
		t.Errorf("policyName = %q, want %q (the sibling opts into p and satisfies p's own selector)", got, "p")
	}
}

func TestGetWorkloadPolicyAnnotation_PoliciesListFails(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	// The Policy exists, so only the List path can fail here.
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*sustainv1alpha1.PolicyList); ok {
					return errors.New("apiserver is down")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	_, err := srv.getWorkloadPolicyAnnotation(context.Background(), "default", "Deployment", "web")
	if err == nil {
		t.Fatalf("expected an error when the Policy List fails")
	}
}

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

func TestRecommendationsAbsoluteRange(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d, policy).Build()
	prom := &fakePromClient{}
	srv := &Server{K8sClient: c, PromClient: prom, Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadRecommendations(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/recommendations?from=1718000000&to=1718003600&step=5m", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := prom.capturedRecRange
	if got.Start.Unix() != 1718000000 || got.End.Unix() != 1718003600 {
		t.Errorf("recommendation range = [%d,%d], want [1718000000,1718003600]",
			got.Start.Unix(), got.End.Unix())
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
	srv := &Server{K8sClient: c, PromClient: prom, Logger: testLogger(t)}

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
