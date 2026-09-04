package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	whhandler "github.com/noony/k8s-sustain/internal/webhook"
	"github.com/noony/k8s-sustain/internal/workload"
)

// TestIntegration_ControllerWritesCache_WebhookReadsIt is the contract test
// between the controller (WLR writer) and the webhook (WLR reader) — the
// only two components in the recommendation pipeline now that the webhook
// never queries Prometheus itself. It catches drift in:
//   - WLR object name format (wlrName)
//   - sweep label key (wlrPolicyLabel)
//   - status shape (Containers map, ObservedAt, Source)
//   - staleness threshold (DefaultCacheStaleness)
//
// The webhook reads via its real ServeHTTP entry point — the WLR is its only
// recommendation source, so this is the primary path, not a fallback.
func TestIntegration_ControllerWritesCache_WebhookReadsIt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	// Pod → ReplicaSet → Deployment chain so resolveOwner finds Deployment.
	policy := basicOngoingPolicy("intg-policy")
	ctrlTrue := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "web",
				Controller: &ctrlTrue,
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(policy, rs).
		Build()

	// Mirrors what reconcileWorkload does after a successful pass.
	r := &PolicyReconciler{Client: c, Scheme: scheme}
	wantCPU := resource.MustParse("250m")
	wantMem := resource.MustParse("128Mi")
	_ = r.upsertWorkloadRecommendation(context.Background(),
		itemForTarget(&workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}),
		policy.Name,
		map[string]workload.ContainerRecommendation{
			"app": {CPURequest: &wantCPU, MemoryRequest: &wantMem},
		},
		metav1.Now(),
	)

	// Sanity: the WLR landed where the webhook will look for it, and carries
	// the new sweep label so list-by-policy calls find it server-side.
	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &wlr); err != nil {
		t.Fatalf("WLR not written by controller upsert: %v", err)
	}
	if got := wlr.Labels[wlrPolicyLabel]; got != policy.Name {
		t.Errorf("WLR sweep label = %q, want %q", got, policy.Name)
	}

	// The webhook has no Prometheus client at all to fall back from.
	h := &whhandler.Handler{Client: c}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "web-abc123-xyz",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rs.Name,
				Controller: &ctrlTrue,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	resp := sendAdmission(t, h, pod)

	if !resp.Allowed {
		t.Fatalf("admission denied; want allowed (fail-open). result=%v", resp.Result)
	}
	if len(resp.Patch) == 0 {
		t.Fatal("no patch returned; webhook should have injected from the WLR the controller wrote")
	}
	patchStr := string(resp.Patch)
	// Loose substring assertions: the JSONPatch is RFC 6902 ops, values are
	// quantity strings. We just need to see the cached numbers flow through.
	if !strings.Contains(patchStr, `"250m"`) {
		t.Errorf("patch missing cached CPU 250m: %s", patchStr)
	}
	if !strings.Contains(patchStr, `"128Mi"`) {
		t.Errorf("patch missing cached memory 128Mi: %s", patchStr)
	}
}

// Staleness is the webhook's only defense against injecting from a controller
// that has stopped reconciling: an over-age WLR must yield no patch at all.
func TestIntegration_StaleCache_WebhookFallsOpen(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	policy := basicOngoingPolicy("intg-stale")
	ctrlTrue := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Controller: &ctrlTrue,
			}},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(policy, rs).
		Build()

	// Backdate ObservedAt past the webhook's default staleness window.
	r := &PolicyReconciler{Client: c, Scheme: scheme}
	wantCPU := resource.MustParse("250m")
	stale := metav1.NewTime(time.Now().Add(-2 * 24 * time.Hour))
	_ = r.upsertWorkloadRecommendation(context.Background(),
		itemForTarget(&workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}),
		policy.Name,
		map[string]workload.ContainerRecommendation{
			"app": {CPURequest: &wantCPU},
		},
		stale,
	)

	h := &whhandler.Handler{Client: c}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "web-abc123-xyz",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Controller: &ctrlTrue,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	resp := sendAdmission(t, h, pod)
	if !resp.Allowed {
		t.Fatalf("admission denied; want allowed")
	}
	if len(resp.Patch) != 0 {
		t.Errorf("expected no patch on stale cache; got: %s", string(resp.Patch))
	}
}

// basicOngoingPolicy is a minimal Ongoing-mode Deployment policy that
// matches all namespaces. p95 for both CPU and memory.
func basicOngoingPolicy(name string) *sustainv1alpha1.Policy {
	mode := sustainv1alpha1.UpdateModeOngoing
	p95 := int32(95)
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &mode},
				},
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
			},
		},
	}
}

// sendAdmission wraps a Pod into an AdmissionReview, runs it through the
// webhook handler's ServeHTTP, and returns the parsed AdmissionResponse.
func sendAdmission(t *testing.T, h *whhandler.Handler, pod *corev1.Pod) *admissionv1.AdmissionResponse {
	t.Helper()

	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook HTTP status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out admissionv1.AdmissionReview
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode admission response: %v", err)
	}
	if out.Response == nil {
		t.Fatal("no Response in AdmissionReview")
	}
	return out.Response
}
