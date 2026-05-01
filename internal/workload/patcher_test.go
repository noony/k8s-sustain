package workload

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRecyclePods_ExposesPublicMethod(t *testing.T) {
	p := New(nil, false)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "test"},
	})

	defer func() {
		if r := recover(); r != nil {
			// nil client causes a panic when listing pods — that's expected
			// and confirms RecyclePods delegates to the real implementation.
			t.Logf("recovered expected panic: %v", r)
		}
	}()

	err := p.RecyclePods(context.Background(), "default", sel, nil)
	if err == nil {
		t.Error("expected error with nil client")
	}
}

func qtyp(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func TestApplyRecommendations_AlwaysApplies(t *testing.T) {
	containers := []corev1.Container{
		{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {CPURequest: qtyp("200m"), MemoryRequest: qtyp("64Mi")},
	}

	out, changed := applyRecommendations(containers, recs)
	if !changed {
		t.Error("expected change")
	}
	if out[0].Resources.Requests.Cpu().Cmp(resource.MustParse("200m")) != 0 {
		t.Errorf("expected 200m CPU, got %s", out[0].Resources.Requests.Cpu())
	}
	if out[0].Resources.Requests.Memory().Cmp(resource.MustParse("64Mi")) != 0 {
		t.Errorf("expected 64Mi memory, got %s", out[0].Resources.Requests.Memory())
	}
}

func TestApplyRecommendations_SetsWhenNoCPU(t *testing.T) {
	containers := []corev1.Container{
		{Name: "app"},
	}
	recs := map[string]ContainerRecommendation{
		"app": {CPURequest: qtyp("200m")},
	}

	out, changed := applyRecommendations(containers, recs)
	if !changed {
		t.Error("expected change when no CPU request set")
	}
	if out[0].Resources.Requests.Cpu().Cmp(resource.MustParse("200m")) != 0 {
		t.Errorf("expected 200m, got %s", out[0].Resources.Requests.Cpu())
	}
}

func TestApplyRecommendations_RemovesLimit(t *testing.T) {
	containers := []corev1.Container{
		{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {CPURequest: qtyp("100m"), RemoveCPULimit: true},
	}

	out, changed := applyRecommendations(containers, recs)
	if !changed {
		t.Error("expected change")
	}
	if _, exists := out[0].Resources.Limits[corev1.ResourceCPU]; exists {
		t.Error("expected CPU limit to be removed")
	}
}

func TestApplyRecommendations_NoMatchingContainer(t *testing.T) {
	containers := []corev1.Container{
		{Name: "app"},
	}
	recs := map[string]ContainerRecommendation{
		"sidecar": {CPURequest: qtyp("100m")},
	}

	_, changed := applyRecommendations(containers, recs)
	if changed {
		t.Error("expected no change when container names don't match")
	}
}

func TestPodIsStale_DetectsChangedCPU(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				},
			},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {CPURequest: qtyp("200m")},
	}
	if !podIsStale(pod, recs) {
		t.Error("expected pod to be stale")
	}
}

func TestPodIsStale_NotStaleWhenMatching(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("200m"),
						},
					},
				},
			},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {CPURequest: qtyp("200m")},
	}
	if podIsStale(pod, recs) {
		t.Error("expected pod to not be stale")
	}
}

// runningPod is a small builder for pods used by recyclePods tests.
func runningPod(name string, requests corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: requests},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestRecyclePods_Eviction_HappyPath verifies that on a non-in-place cluster
// the patcher iterates running pods and creates an Eviction subresource for
// stale ones. Pods already at target are left alone.
func TestRecyclePods_Eviction_HappyPath(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	fresh := runningPod("fresh", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale, fresh).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = append(evicted, obj.GetName())
				}
				return nil
			},
		}).
		Build()

	p := New(c, false /* not in-place */)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected only 'stale' evicted, got %v", evicted)
	}
}

// TestRecyclePods_SkipsTerminatingAndNonRunning verifies that pods being
// deleted or not in the Running phase are skipped without trying to evict.
func TestRecyclePods_SkipsTerminatingAndNonRunning(t *testing.T) {
	terminating := runningPod("terminating", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	finalizers := []string{"x"}
	terminating.Finalizers = finalizers

	pending := runningPod("pending", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	pending.Status.Phase = corev1.PodPending

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	calls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminating, pending).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					calls++
				}
				return nil
			},
		}).
		Build()

	p := New(c, false)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if calls != 0 {
		t.Errorf("expected zero evictions for terminating/pending pods, got %d", calls)
	}
}

// TestEvictPod_PDBBlocked_ReturnsNil verifies that a 429 from the Eviction API
// (PodDisruptionBudget blocking) is treated as a no-op so the next reconcile
// can retry. The patcher must not return an error in this case.
func TestEvictPod_PDBBlocked_ReturnsNil(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					return apierrors.NewTooManyRequests("PDB blocks eviction", 0)
				}
				return nil
			},
		}).
		Build()

	p := New(c, false)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Errorf("expected nil on PDB block, got %v", err)
	}
}

// TestEvictPod_NotFound_ReturnsNil verifies that evicting a pod which no
// longer exists is treated as a successful no-op.
func TestEvictPod_NotFound_ReturnsNil(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					return apierrors.NewNotFound(corev1.Resource("pods"), "stale")
				}
				return nil
			},
		}).
		Build()

	p := New(c, false)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Errorf("expected nil on NotFound, got %v", err)
	}
}

// TestPatchPodInPlace_HappyPath verifies the in-place path uses the /resize
// subresource patch when available and does not fall back to eviction.
func TestPatchPodInPlace_HappyPath(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled, evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
					return nil
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evictionCalled = true
				}
				return nil
			},
		}).
		Build()

	p := New(c, true /* in-place */)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize subresource patch to be called")
	}
	if evictionCalled {
		t.Error("did not expect eviction in happy path")
	}
}

// TestPatchPodInPlace_InfeasibleFallsBackToEviction verifies that when the
// kubelet has marked the resize as infeasible, the patcher does not retry the
// resize and instead evicts the pod.
func TestPatchPodInPlace_InfeasibleFallsBackToEviction(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	stale.Status.Resize = corev1.PodResizeStatusInfeasible

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled bool
	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = append(evicted, obj.GetName())
				}
				return nil
			},
		}).
		Build()

	p := New(c, true)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if resizeCalled {
		t.Error("did not expect /resize to be called when status is Infeasible")
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected eviction of 'stale', got %v", evicted)
	}
}

// TestPatchPodInPlace_DeferredIsNoOp verifies that when the kubelet has
// deferred the resize, the patcher leaves the pod alone.
func TestPatchPodInPlace_DeferredIsNoOp(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	stale.Status.Resize = corev1.PodResizeStatusDeferred

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled, evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evictionCalled = true
				}
				return nil
			},
		}).
		Build()

	p := New(c, true)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if resizeCalled || evictionCalled {
		t.Errorf("expected no-op when resize Deferred (resize=%v, eviction=%v)", resizeCalled, evictionCalled)
	}
}
