package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qtyp(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func TestModeForKind(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	onCreate := sustainv1alpha1.UpdateModeOnCreate

	ut := sustainv1alpha1.UpdateTypes{
		Deployment:  &ongoing,
		StatefulSet: &onCreate,
		CronJob:     &ongoing,
		ArgoRollout: &onCreate,
	}

	tests := []struct {
		kind string
		want *sustainv1alpha1.UpdateMode
	}{
		{"Deployment", &ongoing},
		{"StatefulSet", &onCreate},
		{"CronJob", &ongoing},
		{"Rollout", &onCreate},
		{"DaemonSet", nil},
		{"Unknown", nil},
	}

	for _, tt := range tests {
		got := ut.ModeForKind(tt.kind)
		if tt.want == nil {
			if got != nil {
				t.Errorf("modeForKind(%q) = %v, want nil", tt.kind, *got)
			}
			continue
		}
		if got == nil || *got != *tt.want {
			t.Errorf("modeForKind(%q) = %v, want %v", tt.kind, got, *tt.want)
		}
	}
}

// Walking Pod → ReplicaSet → Rollout is what makes OnCreate injection work for
// Argo Rollout-owned pods.
func TestResolveOwner_RolloutChain(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	ctrl := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-rollout-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "argoproj.io/v1alpha1",
				Kind:       "Rollout",
				Name:       "my-rollout",
				Controller: &ctrl,
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	h := &Handler{Client: fakeClient}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-rollout-abc123-xyz",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "my-rollout-abc123",
				Controller: &ctrl,
			}},
		},
	}

	kind, name, err := workload.ResolvePodOwner(context.Background(), h.Client, pod)
	if err != nil {
		t.Fatalf("resolveOwner: %v", err)
	}
	if kind != "Rollout" {
		t.Errorf("kind = %q, want %q", kind, "Rollout")
	}
	if name != "my-rollout" {
		t.Errorf("name = %q, want %q", name, "my-rollout")
	}
}

func TestResolveOwner_DeploymentChain(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	ctrl := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-deploy-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-deploy",
				Controller: &ctrl,
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	h := &Handler{Client: fakeClient}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-deploy-abc123-xyz",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "my-deploy-abc123",
				Controller: &ctrl,
			}},
		},
	}

	kind, name, err := workload.ResolvePodOwner(context.Background(), h.Client, pod)
	if err != nil {
		t.Fatalf("resolveOwner: %v", err)
	}
	if kind != "Deployment" {
		t.Errorf("kind = %q, want %q", kind, "Deployment")
	}
	if name != "my-deploy" {
		t.Errorf("name = %q, want %q", name, "my-deploy")
	}
}

// admitTestEnv bundles the boilerplate for end-to-end admit() tests. There is
// no Prometheus mock: tests that need an injection seed a WorkloadRecommendation
// via objs, using freshWLR below.
type admitTestEnv struct {
	handler *Handler
}

// newAdmitEnv builds a Handler backed by a fake client preloaded with objs
// (Policies, ReplicaSets, WorkloadRecommendations, ...).
func newAdmitEnv(t *testing.T, objs ...runtime.Object) *admitTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	objsTyped := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if co, ok := o.(client.Object); ok {
			objsTyped = append(objsTyped, co)
		}
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objsTyped...).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()

	return &admitTestEnv{
		handler: &Handler{Client: fc},
	}
}

// wlrRec builds a single-container recommendation entry from plain quantity
// strings, mirroring what the controller's upsert path writes.
func wlrRec(cpu, mem string) sustainv1alpha1.ContainerRecommendation {
	return sustainv1alpha1.ContainerRecommendation{CPURequest: qtyp(cpu), MemoryRequest: qtyp(mem)}
}

// freshWLR builds a WorkloadRecommendation for (kind, ns, name) with a fresh
// ObservedAt, keyed exactly as the controller writes and the webhook reads it.
// freshWLR is a just-written WorkloadRecommendation produced under policy "p",
// the Policy the admission tests' pods opt into.
func freshWLR(kind, ns, name string, containers map[string]sustainv1alpha1.ContainerRecommendation) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: wlrcache.Name(kind, name)},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: ns, Name: name},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.Now(),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			ComputedBy: "p",
			Containers: containers,
		},
	}
}

func basicPolicy(name string, mode sustainv1alpha1.UpdateMode) *sustainv1alpha1.Policy {
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

func deploymentReplicaSet(ns, rsName, deployName string) *appsv1.ReplicaSet {
	ctrl := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      rsName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
				Controller: &ctrl,
			}},
		},
	}
}

func podWithRSOwner(ns, podName, rsName, policy string) *corev1.Pod {
	ctrl := true
	annotations := map[string]string{}
	if policy != "" {
		annotations[sustainv1alpha1.PolicyAnnotation] = policy
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        podName,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rsName,
				Controller: &ctrl,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
}

func admissionRequestFor(t *testing.T, pod *corev1.Pod) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return &admissionv1.AdmissionRequest{
		UID:       "uid-1",
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		Object:    runtime.RawExtension{Raw: raw},
	}
}

func TestServeHTTP_RoundTripsAdmissionReview(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1"},
		Request:  admissionRequestFor(t, pod),
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Response == nil {
		t.Fatal("response missing")
	}
	if out.Response.UID != "uid-1" {
		t.Errorf("UID = %q, want uid-1", out.Response.UID)
	}
	if !out.Response.Allowed {
		t.Error("expected allowed=true")
	}
	if out.Response.Patch == nil {
		t.Error("expected patch in response")
	}
}

func TestServeHTTP_BadBody_Returns400(t *testing.T) {
	env := newAdmitEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A pod annotated on its template reads its owner only for its identity, once
// its Policy accepts it. That read must still go through resolveCachedPodOwner,
// or a rolling restart pays N uncached ReplicaSet Gets on the admission hot
// path.
func TestAdmit_PodTemplateAnnotated_ConcurrentAdmissionsCollapseToOneReplicaSetGet(t *testing.T) {
	const n = 50
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("team-a", "web-abc", "web")
	wlr := freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})

	var joined int32
	allJoined := make(chan struct{})
	joinHook := func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	var rsGets int32
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(policy, rs, wlr).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					atomic.AddInt32(&rsGets, 1)
					<-allJoined // held open until every one of the N admissions has joined
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c, sfJoinHook: joinHook}

	reqs := make([]*admissionv1.AdmissionRequest, n)
	for i := range n {
		pod := podWithRSOwner("team-a", fmt.Sprintf("web-abc-%d", i), "web-abc", "p")
		reqs[i] = admissionRequestFor(t, pod)
	}

	var wg sync.WaitGroup
	allowed := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := h.admit(context.Background(), reqs[i])
			allowed[i] = resp.Allowed
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&rsGets); got != 1 {
		t.Errorf("expected exactly 1 ReplicaSet Get for %d pod-template-annotated admissions behind the same owner, got %d", n, got)
	}
	for i, a := range allowed {
		if !a {
			t.Errorf("admission %d not allowed", i)
		}
	}
}

// A Policy's label selector matches the pod template's labels for the
// controller's inventory and the webhook alike. Labels only on the
// Deployment's own metadata never reach its pods, so neither governs it; had
// they disagreed, an Ongoing workload would be evicted every pass and come
// back unmutated.
func TestPolicySelectorMatchesThePodTemplateForInventoryAndWebhook(t *testing.T) {
	tierWeb := map[string]string{"tier": "web"}
	for _, tc := range []struct {
		name       string
		onTemplate bool
	}{
		{name: "labels on the Deployment only"},
		{name: "labels on the pod template", onTemplate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := basicPolicy("p", sustainv1alpha1.UpdateModeOngoing)
			policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: tierWeb}
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "my-app"}}
			d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
			if tc.onTemplate {
				d.Spec.Template.Labels = tierWeb
			} else {
				d.Labels = tierWeb
			}
			c := fake.NewClientBuilder().WithScheme(config.Scheme()).
				WithObjects(policy, d, deploymentReplicaSet("default", "my-app-rs", "my-app"),
					freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
						"app": wlrRec("100m", "64Mi"),
					})).
				WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()

			snap, err := inventory.Take(context.Background(), c, inventory.Options{})
			if err != nil {
				t.Fatalf("Take: %v", err)
			}
			id, ok := snap.Lookup(promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "my-app"})
			if !ok {
				t.Fatal("the Deployment is no identity")
			}
			pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
			pod.Labels = d.Spec.Template.Labels
			resp := (&Handler{Client: c}).admit(context.Background(), admissionRequestFor(t, pod))

			governed, injected := id.Policy == "p", strings.Contains(string(resp.Patch), "/resources")
			if governed != tc.onTemplate || injected != tc.onTemplate {
				t.Errorf("inventory governs = %v, webhook injects = %v; want both %v", governed, injected, tc.onTemplate)
			}
		})
	}
}
