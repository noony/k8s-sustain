package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
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

// TestResolveOwner_RolloutChain verifies the webhook walks
// Pod → ReplicaSet → Rollout (Argo Rollouts) and reports the Rollout as the
// top-level workload kind. This is what makes OnCreate injection work for
// Rollout-owned pods.
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

// TestResolveOwner_DeploymentChain verifies the existing
// Pod → ReplicaSet → Deployment walk still works alongside the Rollout case.
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

func TestBuildPatches_EmptyRecs(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	recs := map[string]workload.ContainerRecommendation{}
	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil patches for empty recs")
	}
}

func TestBuildPatches_SetsResources(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {
			CPURequest:    qtyp("100m"),
			MemoryRequest: qtyp("64Mi"),
		},
	}

	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected patches, got nil")
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Op != "add" {
		t.Errorf("expected op 'add', got %q", patches[0].Op)
	}
	if patches[0].Path != "/spec/containers/0/resources" {
		t.Errorf("expected path '/spec/containers/0/resources', got %q", patches[0].Path)
	}
}

func TestBuildPatches_MultipleContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app":     {CPURequest: qtyp("100m")},
		"sidecar": {CPURequest: qtyp("50m")},
	}

	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}
}

func TestBuildPatches_SkipsUnmatchedContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qtyp("100m")},
	}

	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
}

// TestBuildPatches_NameCollisionInitAndRegular verifies the corner case where
// the same container name appears in both pod.Spec.Containers and
// pod.Spec.InitContainers. Kubernetes permits this; our model identifies
// recommendations by name, so the single recommendation must be patched onto
// BOTH locations. Locking in this behaviour: changes that silently drop one
// of the two patches would otherwise leave the init copy un-rightsized.
func TestBuildPatches_NameCollisionInitAndRegular(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "shared"}},
			InitContainers: []corev1.Container{{Name: "shared"}},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"shared": {CPURequest: qtyp("123m"), MemoryRequest: qtyp("64Mi")},
	}

	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (one per location), got %d: %s", len(patches), string(result))
	}
	paths := map[string]bool{}
	for _, p := range patches {
		paths[p.Path] = true
	}
	want := []string{
		"/spec/containers/0/resources",
		"/spec/initContainers/0/resources",
	}
	for _, p := range want {
		if !paths[p] {
			t.Errorf("missing patch path %q for name collision (got %v)", p, paths)
		}
	}
}

// TestBuildPatches_PatchesInitContainers verifies that recommendations whose
// names match init containers produce JSON patches at /spec/initContainers/N/resources.
func TestBuildPatches_PatchesInitContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "migrate"}, {Name: "warm-cache"}},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app":        {CPURequest: qtyp("100m")},
		"migrate":    {CPURequest: qtyp("250m")},
		"warm-cache": {CPURequest: qtyp("50m")},
	}

	result, err := buildPatches(pod, recs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 3 {
		t.Fatalf("expected 3 patches, got %d", len(patches))
	}
	paths := map[string]bool{}
	for _, p := range patches {
		paths[p.Path] = true
	}
	want := []string{
		"/spec/containers/0/resources",
		"/spec/initContainers/0/resources",
		"/spec/initContainers/1/resources",
	}
	for _, p := range want {
		if !paths[p] {
			t.Errorf("missing patch path %q (got %v)", p, paths)
		}
	}
}

// admitTestEnv bundles the boilerplate for end-to-end admit() tests: scheme,
// fake client, and a constructed Handler. The webhook reads recommendations
// exclusively from a WorkloadRecommendation now — there is no Prometheus
// mock to configure. Tests that need admission to inject something seed one
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
// ObservedAt, keyed exactly as the controller writes and the webhook reads
// (wlrName). Admission tests seed one of these instead of configuring a
// Prometheus mock — the webhook's only recommendation source now.
func freshWLR(kind, ns, name string, containers map[string]sustainv1alpha1.ContainerRecommendation) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: wlrName(kind, name)},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.Now(),
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

// TestAdmit_EmptyObjectRaw_AllowsWithoutPatch verifies the handler fails open
// when AdmissionRequest.Object.Raw is nil/empty (a malformed apiserver review
// or an unrelated subresource where Object is not populated).
func TestAdmit_EmptyObjectRaw_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t)

	resp := env.handler.admit(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "uid",
		Namespace: "default",
		Name:      "n",
		Kind:      metav1.GroupVersionKind{Kind: "Pod"},
		Object:    runtime.RawExtension{}, // Raw is nil
	})
	if !resp.Allowed {
		t.Fatal("expected allow on empty Object.Raw")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_NoAnnotation_AllowsWithoutPatch verifies pods without the policy
// annotation pass through untouched.
func TestAdmit_NoAnnotation_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t)

	pod := podWithRSOwner("default", "p", "rs", "")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_PolicyNotFound_AllowsWithoutPatch verifies fail-open behaviour
// when the annotation references a Policy that does not exist.
func TestAdmit_PolicyNotFound_AllowsWithoutPatch(t *testing.T) {
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, rs)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "missing-policy")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch when policy missing, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_StandalonePod_AllowsWithoutPatch verifies pods without a controller
// owner are skipped — the webhook can't determine workload kind.
func TestAdmit_StandalonePod_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t, basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "standalone",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Error("expected no patch for standalone pod")
	}
}

// TestAdmit_KindNotConfigured_AllowsWithoutPatch verifies that a workload kind
// not listed in the policy's update.types is skipped.
func TestAdmit_KindNotConfigured_AllowsWithoutPatch(t *testing.T) {
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				// Only StatefulSet configured — Deployment-owned pods should be skipped.
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{StatefulSet: &mode}},
			},
		},
	}
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Error("expected no patch when kind not configured")
	}
}

// TestAdmit_RecommendOnly_AllowsWithoutPatch verifies that recommend-only
// mode returns allow=true with no patch even when injection would have applied.
func TestAdmit_RecommendOnly_AllowsWithoutPatch(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	// A WLR is present with data to inject, so the assertion below actually
	// exercises the recommend-only short-circuit rather than the (also
	// no-patch) "nothing to inject" path.
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)
	env.handler.RecommendOnly = true

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("recommend-only must not patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_PolicyRecommendOnly_AllowsWithoutPatch verifies that a Policy
// with spec.rightSizing.recommendOnly=true is admitted without a resource
// patch even though the handler's global RecommendOnly flag is off.
func TestAdmit_PolicyRecommendOnly_AllowsWithoutPatch(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.RightSizing.RecommendOnly = true
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("policy recommend-only must not patch, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_BarePodWithOwnerName_InjectsAsPodKind verifies a pod with no
// controller owner but a valid owner-name annotation is treated as kind
// "Pod": it gets the label-mirror patch AND, when the policy configures
// types.pod, a resource injection patch read from the WorkloadRecommendation
// cached under the overridden identity.
func TestAdmit_BarePodWithOwnerName_InjectsAsPodKind(t *testing.T) {
	mode := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Pod: &mode}},
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To(int32(95))}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To(int32(95))}},
				},
			},
		},
	}
	wlr := freshWLR("Pod", "default", "etl-daily", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, wlr)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "etl-daily-run-1",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected a patch (label mirror + resource injection)")
	}
	patchStr := string(resp.Patch)
	if !strings.Contains(patchStr, `"/metadata/labels"`) && !strings.Contains(patchStr, `k8s.sustain.io~1owner-name`) {
		t.Errorf("expected a label patch in %s", patchStr)
	}
	if !strings.Contains(patchStr, `"/spec/containers/0/resources"`) {
		t.Errorf("expected a resource injection patch in %s", patchStr)
	}
}

// TestAdmit_BarePodWithOwnerName_KindNotConfigured_LabelOnly verifies that
// when the policy does not configure types.pod, a bare pod with a valid
// owner-name annotation still gets the label mirror patch, but no resource
// injection (mirrors TestAdmit_KindNotConfigured_AllowsWithoutPatch for every
// other kind).
func TestAdmit_BarePodWithOwnerName_KindNotConfigured_LabelOnly(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate) // only Deployment configured
	env := newAdmitEnv(t, policy)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "etl-daily-run-1",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected the label-mirror patch even though types.pod is unconfigured")
	}
	patchStr := string(resp.Patch)
	if strings.Contains(patchStr, "resources") {
		t.Errorf("did not expect a resource injection patch, got %s", patchStr)
	}
}

// TestAdmit_OwnedPodWithOwnerNameOverride_ReadsOverriddenIdentityWLR verifies
// that a pod with a real controller owner AND a valid owner-name annotation
// still gets the label-mirror patch, and that the WorkloadRecommendation read
// is keyed by the overridden name, not the real Deployment name: two WLRs are
// seeded (one under each name), and the injected values must come from the
// overridden one only.
func TestAdmit_OwnedPodWithOwnerNameOverride_ReadsOverriddenIdentityWLR(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "app-blue-rs", "app-blue")
	realWLR := freshWLR("Deployment", "default", "app-blue", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("999m", "999Mi"),
	})
	overrideWLR := freshWLR("Deployment", "default", "app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, realWLR, overrideWLR)

	pod := podWithRSOwner("default", "app-blue-rs-xyz", "app-blue-rs", "p")
	pod.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "app"
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected an injection patch")
	}
	patchStr := string(resp.Patch)
	if !strings.Contains(patchStr, `"100m"`) {
		t.Errorf("expected injection from the overridden identity's WLR (100m): %s", patchStr)
	}
	if strings.Contains(patchStr, `"999m"`) {
		t.Errorf("must not inject from the real, non-overridden identity's WLR: %s", patchStr)
	}
}

// TestAdmit_RecommendOnly_OwnerNameAnnotation_LabelPatchOnly verifies
// RecommendOnly's "never mutates the pod" contract is scoped to
// resources/limits: a valid owner-name annotation still produces the
// label-mirror patch (the only mechanism that makes the override visible to
// Prometheus via kube-state-metrics), but never a resource-injection patch.
func TestAdmit_RecommendOnly_OwnerNameAnnotation_LabelPatchOnly(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)
	env.handler.RecommendOnly = true

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	pod.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "app"
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected the owner-name label-mirror patch even under RecommendOnly")
	}
	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected exactly one patch (label mirror only, no resource injection), got %d: %v", len(patches), patches)
	}
	// The pod has nil Labels, so the mirror patch adds the whole map rather
	// than a single nested key — see the pod.Labels == nil branch in admit().
	if patches[0].Path != "/metadata/labels" {
		t.Errorf("expected the label-mirror patch, got path %q", patches[0].Path)
	}
	for _, p := range patches {
		if strings.Contains(p.Path, "/resources") {
			t.Errorf("recommend-only must not inject resources, got patch at %q", p.Path)
		}
	}
}

// TestAdmit_InvalidOwnerNameLabelValue_NoLabelPatch verifies an owner-name
// annotation value that fails Kubernetes label-value validation is treated
// as absent: no label patch, today's owner-resolution behavior unchanged.
func TestAdmit_InvalidOwnerNameLabelValue_NoLabelPatch(t *testing.T) {
	env := newAdmitEnv(t, basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "standalone",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "Invalid/Value",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch for invalid owner-name value, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_DeploymentInjection_PatchesResources verifies the happy-path:
// annotated pod owned by a Deployment-backed ReplicaSet gets a JSON Patch
// setting CPU and memory requests for the matching container.
func TestAdmit_DeploymentInjection_PatchesResources(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected JSON patch for happy-path injection")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("expected JSONPatch type, got %v", resp.PatchType)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch op, got %d", len(patches))
	}
	if patches[0].Path != "/spec/containers/0/resources" {
		t.Errorf("patch path = %q", patches[0].Path)
	}
}

// TestServeHTTP_RoundTripsAdmissionReview verifies the HTTP handler decodes
// the request, runs admit, and re-encodes a valid AdmissionReview response
// keyed by the original UID.
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

// TestServeHTTP_BadBody_Returns400 verifies the handler rejects malformed
// AdmissionReview JSON with HTTP 400 instead of allowing through.
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

func TestIsValidPolicyName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"good", true},
		{"good-name-123", true},
		{"", false},
		{"UPPER", false},                  // DNS-1123 is lowercase
		{"a/b", false},                    // slash
		{strings.Repeat("a", 254), false}, // > 253 chars
		{"-leading-dash", false},
		{"trailing-dash-", false},
		{"a..b", false}, // empty label
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isValidPolicyName(c.name)
			if got != c.want {
				t.Errorf("isValidPolicyName(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestAdmit_InvalidPolicyAnnotation_AllowsWithoutPatch verifies that a
// malformed annotation value (uppercase, oversized, etc.) is rejected early,
// before it is ever used to look up a Policy object.
func TestAdmit_InvalidPolicyAnnotation_AllowsWithoutPatch(t *testing.T) {
	env := newAdmitEnv(t, basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate))

	pod := podWithRSOwner("default", "p", "rs", strings.Repeat("a", 300))
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow for invalid annotation")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch for invalid policy name, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_PolicySelectorNamespaces_PodOutsideListSkipped verifies that the
// webhook honours Policy.Spec.Selector.Namespaces: a pod in a namespace not
// listed by the policy is admitted without mutation.
func TestAdmit_PolicySelectorNamespaces_PodOutsideListSkipped(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.Selector.Namespaces = []string{"production"}

	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow (fail-open) for pod outside selector.namespaces")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch when pod ns not in selector.namespaces, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_PolicySelectorNamespaces_PodInsideListInjected verifies the
// positive case: a pod in a namespace listed by the policy IS mutated.
func TestAdmit_PolicySelectorNamespaces_PodInsideListInjected(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.Selector.Namespaces = []string{"production"}

	rs := deploymentReplicaSet("production", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "production", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)

	pod := podWithRSOwner("production", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected patch when pod ns in selector.namespaces")
	}
}

// TestAdmit_ExcludedNamespaces_Skipped verifies the webhook respects its
// configured --excluded-namespaces list: a pod in an excluded ns is admitted
// without mutation regardless of selector configuration.
func TestAdmit_ExcludedNamespaces_Skipped(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)

	rs := deploymentReplicaSet("kube-system", "kube-app-rs", "kube-app")
	env := newAdmitEnv(t, policy, rs)
	env.handler.ExcludedNamespaces = []string{"kube-system"}

	pod := podWithRSOwner("kube-system", "kube-app-rs-xyz", "kube-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow (fail-open) for excluded namespace")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch for excluded namespace, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_LabelSelector_PodLabelsDontMatch_Skipped verifies the webhook
// honours Policy.Spec.Selector.LabelSelector: a pod whose labels do not match
// is admitted without mutation.
func TestAdmit_LabelSelector_PodLabelsDontMatch_Skipped(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"team": "platform"},
	}

	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	pod.Labels = map[string]string{"team": "growth"}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow for non-matching labels")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch when labels don't match, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_LabelSelector_PodLabelsMatch_Injected verifies the positive case:
// a pod whose labels match the selector IS mutated.
func TestAdmit_LabelSelector_PodLabelsMatch_Injected(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"team": "platform"},
	}

	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	pod.Labels = map[string]string{"team": "platform"}
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("expected allow")
	}
	if resp.Patch == nil {
		t.Fatal("expected patch when labels match selector")
	}
}

// TestAdmit_InvalidLabelSelector_FailsOpen verifies the webhook fails open on
// malformed selectors — the pod is admitted without mutation, never denied.
func TestAdmit_InvalidLabelSelector_FailsOpen(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      "app",
			Operator: "InvalidOperator",
		}},
	}

	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs)

	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatal("webhook must fail open (allow) on invalid selector")
	}
	if resp.Patch != nil {
		t.Errorf("must not patch when selector is malformed, got %d bytes", len(resp.Patch))
	}
}

// TestAdmit_NamespaceLevelOptIn_Injects verifies the full admission path for a
// pod whose only opt-in is on its Namespace: the webhook must resolve it, find
// the cached WorkloadRecommendation, and patch the pod.
func TestAdmit_NamespaceLevelOptIn_Injects(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	env := newAdmitEnv(t,
		basicPolicy("p", sustainv1alpha1.UpdateModeOngoing),
		ns, d,
		deploymentReplicaSet("team-a", "web-abc", "web"),
		freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
			"app": wlrRec("100m", "128Mi"),
		}),
	)
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatalf("pod must always be allowed")
	}
	if len(resp.Patch) == 0 {
		t.Fatalf("expected a resource patch for a namespace-opted-in pod, got none")
	}
}

// TestAdmit_WorkloadOptOutBeatsNamespaceOptIn verifies the escape hatch end to
// end: the namespace opts everything in, the Deployment opts back out, no patch.
func TestAdmit_WorkloadOptOutBeatsNamespaceOptIn(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "web",
		Annotations: map[string]string{sustainv1alpha1.OptOutAnnotation: "true"},
	}}
	env := newAdmitEnv(t,
		basicPolicy("p", sustainv1alpha1.UpdateModeOngoing),
		ns, d,
		deploymentReplicaSet("team-a", "web-abc", "web"),
		freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
			"app": wlrRec("100m", "128Mi"),
		}),
	)
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatalf("pod must always be allowed")
	}
	if len(resp.Patch) != 0 {
		t.Fatalf("an opted-out workload must not be patched, got %s", resp.Patch)
	}
}

// TestAdmit_PodLevelOptOutBeatsNamespaceOptIn verifies the escape hatch at
// the MOST specific level end to end: a pod carrying its own opt-out
// annotation is admitted without a resource patch even though its Namespace
// opts everything in. This is a distinct path from
// TestAdmit_WorkloadOptOutBeatsNamespaceOptIn (the workload-level opt-out):
// a pod-level opt-out is caught by admit() itself, directly off
// pod.Annotations, before resolveOptIn (and therefore before any owner or
// Namespace read) is ever reached — see the comment on that early return in
// handler.go. Only the workload-level opt-out was covered end to end before
// this test.
func TestAdmit_PodLevelOptOutBeatsNamespaceOptIn(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	env := newAdmitEnv(t,
		basicPolicy("p", sustainv1alpha1.UpdateModeOngoing),
		ns, d,
		deploymentReplicaSet("team-a", "web-abc", "web"),
		freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
			"app": wlrRec("100m", "128Mi"),
		}),
	)
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")
	pod.Annotations[sustainv1alpha1.OptOutAnnotation] = "true"

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatalf("pod must always be allowed")
	}
	if len(resp.Patch) != 0 {
		t.Fatalf("a pod-level opt-out must not be patched, got %s", resp.Patch)
	}
}

// TestAdmit_OwnerGetFailure_FailsOpen verifies the new lookup keeps the
// webhook's fail-open contract: a broken read admits the pod unmutated.
func TestAdmit_OwnerGetFailure_FailsOpen(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithObjects(
			basicPolicy("p", sustainv1alpha1.UpdateModeOngoing),
			ns,
			deploymentReplicaSet("team-a", "web-abc", "web"),
			// A WLR with data to inject, so the non-error path would produce a
			// patch: without this, the assertions below would pass whether or
			// not the injected Deployment-Get error actually fired (the
			// no-WLR path is ALSO an allow-with-no-patch), and the test would
			// not discriminate between the two.
			freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
				"app": wlrRec("100m", "128Mi"),
			}),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return errors.New("boom")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")

	resp := h.admit(context.Background(), admissionRequestFor(t, pod))
	if !resp.Allowed {
		t.Fatalf("a failed owner read must still admit the pod")
	}
	if len(resp.Patch) != 0 {
		t.Fatalf("expected no patch when opt-in resolution failed, got %s", resp.Patch)
	}
}

// TestAdmit_PodTemplateAnnotated_ConcurrentAdmissionsCollapseToOneReplicaSetGet
// pins the most valuable of the caching fixes: every EXISTING user annotates
// the pod template directly (sustainv1alpha1.PolicyAnnotation on the pod
// itself), so admit() never calls resolveOptIn for them at all — the
// multi-level opt-in chain (and its caching) is skipped entirely once
// policyName is already non-empty from the pod's own annotation. That left
// the pod-template-annotated path — the one every existing user takes —
// resolving its owner via the uncached workload.ResolvePodOwner, so a
// rolling restart of a pod-template-annotated Deployment still paid N
// uncached ReplicaSet Gets on the admission hot path even after
// resolveOptIn's own walk was cached. admit() must now go through
// h.resolveCachedPodOwner for this path too, and it must do so without
// resolving the owner twice or skipping resolution — see the
// "Already resolved by the multi-level opt-in chain" comment in admit().
func TestAdmit_PodTemplateAnnotated_ConcurrentAdmissionsCollapseToOneReplicaSetGet(t *testing.T) {
	const n = 50
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("team-a", "web-abc", "web")
	wlr := freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})

	var joined int32
	allJoined := make(chan struct{})
	prev := singleflightTestHook
	singleflightTestHook = func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}
	defer func() { singleflightTestHook = prev }()

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
	h := &Handler{Client: c}

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
