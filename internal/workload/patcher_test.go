package workload

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// testEvictionOpts returns Patcher options that keep tests fast: tight poll
// interval and short timeout so waitForReplacement either returns at the next
// poll (replacement up) or surfaces an obvious test bug rather than hanging.
func testEvictionOpts() []Option {
	return []Option{
		WithReadyPollInterval(5 * time.Millisecond),
		WithReadyTimeout(500 * time.Millisecond),
	}
}

// evictionInterceptor returns an interceptor.Funcs that mimics apiserver
// behavior: when an Eviction subresource is created, the pod is deleted from
// the fake client's live store via the inner client. evictedNames receives
// the name of every evicted pod in order.
func evictionInterceptor(evictedNames *[]string) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
			if sub != "eviction" {
				return nil
			}
			*evictedNames = append(*evictedNames, obj.GetName())
			return inner.Delete(ctx, obj)
		},
	}
}

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

	out, changed := applyRecsFiltered(containers, recs, nil)
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

	out, changed := applyRecsFiltered(containers, recs, nil)
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

	out, changed := applyRecsFiltered(containers, recs, nil)
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

	_, changed := applyRecsFiltered(containers, recs, nil)
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

// TestPodIsStale_RestartableInitContainerDriftCountsAsStale verifies that
// drift in a sidecar (restartable init) container's resources triggers a
// pod recycle, since sidecars run for the pod's lifetime.
func TestPodIsStale_RestartableInitContainerDriftCountsAsStale(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}},
			}},
			InitContainers: []corev1.Container{{
				Name:          "sidecar",
				RestartPolicy: &always,
				Resources:     corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
			}},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app":     {CPURequest: qtyp("200m")},
		"sidecar": {CPURequest: qtyp("100m")},
	}
	if !podIsStale(pod, recs) {
		t.Error("expected pod to be stale due to sidecar drift")
	}
}

// TestPodIsStale_DetectsChangedLimit verifies that a recommendation that only
// changes a container's limit (request unchanged) still marks the pod stale.
// Without this the eviction-mode reconcile would skip the pod and the stale
// limit would persist indefinitely.
func TestPodIsStale_DetectsChangedLimit(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
			}},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {
			MemoryRequest: qtyp("256Mi"),
			MemoryLimit:   qtyp("1Gi"),
		},
	}
	if !podIsStale(pod, recs) {
		t.Error("expected pod to be stale: recommended limit differs from current")
	}
}

// TestPodIsStale_DetectsRemovedLimit verifies that a recommendation asking to
// remove a limit (RemoveCPULimit) marks the pod stale when the container still
// has the limit set.
func TestPodIsStale_DetectsRemovedLimit(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app": {
			CPURequest:     qtyp("100m"),
			RemoveCPULimit: true,
		},
	}
	if !podIsStale(pod, recs) {
		t.Error("expected pod to be stale: recommendation removes a limit still present on the container")
	}
}

// TestPodIsStale_ClassicInitContainerDriftIsIgnored verifies that drift in a
// classic (non-restartable) init container does NOT trigger a pod recycle —
// the init container has already exited by the time the pod is Running, and
// the new requests will land via webhook injection on the next pod creation.
func TestPodIsStale_ClassicInitContainerDriftIsIgnored(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}},
			}},
			InitContainers: []corev1.Container{{
				Name:      "migrate",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
			}},
		},
	}
	recs := map[string]ContainerRecommendation{
		"app":     {CPURequest: qtyp("200m")},
		"migrate": {CPURequest: qtyp("100m")},
	}
	if podIsStale(pod, recs) {
		t.Error("expected pod to NOT be stale: classic init container drift should not trigger recycle")
	}
}

// TestApplyRecommendationsToSidecars_OnlyMutatesRestartableContainers verifies
// the sidecar-only path skips classic init containers (which have already exited)
// while updating restartable ones.
func TestApplyRecommendationsToSidecars_OnlyMutatesRestartableContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	in := []corev1.Container{
		{
			Name:          "sidecar",
			RestartPolicy: &always,
			Resources:     corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
		},
		{
			Name:      "migrate",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
		},
	}
	recs := map[string]ContainerRecommendation{
		"sidecar": {CPURequest: qtyp("100m")},
		"migrate": {CPURequest: qtyp("100m")},
	}
	out, changed := applyRecsFiltered(in, recs, isRestartableInitContainer)
	if !changed {
		t.Fatal("expected change for sidecar")
	}
	if got := out[0].Resources.Requests.Cpu().String(); got != "100m" {
		t.Errorf("sidecar CPU: got %s, want 100m", got)
	}
	if got := out[1].Resources.Requests.Cpu().String(); got != "50m" {
		t.Errorf("classic init CPU: got %s, want unchanged 50m", got)
	}
}

// runningPod is a small builder for pods used by recyclePods tests. The pod
// is Running and has PodReady=True so the eviction loop's post-eviction wait
// sees the workload as quiescent — overriding the Conditions in a specific
// test lets it simulate a Pending replacement or a NotReady peer.
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
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
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
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false /* not in-place */, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected only 'stale' evicted, got %v", evicted)
	}
}

// TestRecyclePods_SkipsTerminatingAndTerminal verifies that pods being deleted
// or already in Succeeded/Failed phase are skipped. Pending pods are eligible
// for eviction so a pod stuck Pending because its request is too large gets
// recycled — the webhook re-injects the smaller recommendation on the next
// scheduling attempt.
func TestRecyclePods_SkipsTerminatingAndTerminal(t *testing.T) {
	terminating := runningPod("terminating", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	finalizers := []string{"x"}
	terminating.Finalizers = finalizers

	succeeded := runningPod("succeeded", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	succeeded.Status.Phase = corev1.PodSucceeded

	failed := runningPod("failed", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	failed.Status.Phase = corev1.PodFailed

	pending := runningPod("pending", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	pending.Status.Phase = corev1.PodPending

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminating, succeeded, failed, pending).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "pending" {
		t.Errorf("expected only the Pending pod to be evicted, got %v", evicted)
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
			SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = append(evicted, obj.GetName())
					return inner.Delete(ctx, obj)
				}
				return nil
			},
		}).
		Build()

	p := New(c, true, testEvictionOpts()...)
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

// TestPatchPodInPlace_ResizeNotFoundFallsBackToDirectPatch verifies the
// k8s 1.31–1.32 fallback: when the /resize subresource doesn't exist yet
// (NotFound), the patcher patches the pod directly instead. Eviction must
// not be triggered — direct patch is itself the in-place path on those
// versions.
func TestPatchPodInPlace_ResizeNotFoundFallsBackToDirectPatch(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled, podPatchCalled, evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
					return apierrors.NewNotFound(corev1.Resource("pods"), "resize")
				}
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				podPatchCalled = true
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
	if !resizeCalled {
		t.Error("expected /resize attempt before fallback")
	}
	if !podPatchCalled {
		t.Error("expected direct pod patch fallback when /resize returns NotFound")
	}
	if evictionCalled {
		t.Error("did not expect eviction on NotFound — direct patch is the fallback")
	}
}

// TestPatchPodInPlace_ResizeInvalidFallsBackToEviction verifies the
// feature-gate-disabled fallback: when the API server rejects the resource
// patch as Invalid (InPlacePodVerticalScaling gate off), the patcher gives
// up on in-place for this reconcile and falls back to eviction. The
// inPlace flag should be flipped off so subsequent pods in the same batch
// skip /resize entirely.
func TestPatchPodInPlace_ResizeInvalidFallsBackToEviction(t *testing.T) {
	stale1 := runningPod("stale1", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	stale2 := runningPod("stale2", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalls int
	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale1, stale2).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalls++
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "stale", nil)
				}
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				// Direct pod patch should NOT be tried for IsInvalid (only IsNotFound).
				return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "stale", nil)
			},
			SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = append(evicted, obj.GetName())
					return inner.Delete(ctx, obj)
				}
				return nil
			},
		}).
		Build()

	p := New(c, true, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if len(evicted) != 2 {
		t.Errorf("expected both pods to be evicted after IsInvalid; got %v", evicted)
	}
	// Once the patcher learns the gate is off, p.inPlace flips to false so
	// the SECOND pod's /resize is never tried again. Only one /resize call.
	if resizeCalls != 1 {
		t.Errorf("expected exactly 1 /resize call (then gate detection disables in-place), got %d", resizeCalls)
	}
	if p.InPlace() {
		t.Error("expected p.inPlace to be flipped off after IsInvalid")
	}
}

// TestPatchPodInPlace_SidecarResizeRejected_BestEffort verifies that a
// sidecar resize failure (older clusters / disabled gate for sidecars) does
// NOT fail the reconcile. Regular containers must still be patched in place.
func TestPatchPodInPlace_SidecarResizeRejected_BestEffort(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "stale-with-sidecar",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				},
			}},
			InitContainers: []corev1.Container{{
				Name:          "sidecar",
				RestartPolicy: &always,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var regularResizeOK, sidecarResizeAttempted, evicted bool
	resizeCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub != "resize" {
					return nil
				}
				resizeCalls++
				switch resizeCalls {
				case 1:
					// First call = regular containers. Succeed.
					regularResizeOK = true
					return nil
				default:
					// Second call = sidecars. Older clusters reject this.
					sidecarResizeAttempted = true
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), stale.Name, nil)
				}
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = true
				}
				return nil
			},
		}).
		Build()

	p := New(c, true)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{
		"app":     {CPURequest: qtyp("200m")},
		"sidecar": {CPURequest: qtyp("100m")},
	}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("sidecar rejection should be best-effort, got: %v", err)
	}
	if !regularResizeOK {
		t.Error("regular-container /resize should have succeeded")
	}
	if !sidecarResizeAttempted {
		t.Error("sidecar /resize should have been attempted")
	}
	if evicted {
		t.Error("sidecar rejection should NOT cascade to eviction of the whole pod")
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

// TestResizePodsInPlace_HappyPath verifies that ResizePodsInPlace patches
// the /resize subresource for each Running, non-terminating pod and never
// triggers eviction.
func TestResizePodsInPlace_HappyPath(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs); err != nil {
		t.Fatalf("ResizePodsInPlace: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize subresource patch to be called")
	}
	if evictionCalled {
		t.Error("ResizePodsInPlace must never evict")
	}
}

// TestResizePodsInPlace_NoOpWhenInPlaceDisabled verifies that when the
// patcher was constructed with inPlace=false (older cluster), ResizePodsInPlace
// silently no-ops instead of falling back to eviction. Job pods must finish
// on their existing resources; the next run inherits new resources via the
// webhook.
func TestResizePodsInPlace_NoOpWhenInPlaceDisabled(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var anyCall bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				anyCall = true
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				anyCall = true
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				anyCall = true
				return nil
			},
		}).
		Build()

	p := New(c, false /* inPlace disabled */)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs); err != nil {
		t.Fatalf("ResizePodsInPlace: %v", err)
	}
	if anyCall {
		t.Error("ResizePodsInPlace must no-op (no API calls) when inPlace is disabled")
	}
}

// TestResizePodsInPlace_SkipsTerminatingAndNonRunning verifies that pods
// with a deletion timestamp or non-Running phase are skipped without any
// API call.
func TestResizePodsInPlace_SkipsTerminatingAndNonRunning(t *testing.T) {
	deletionTime := metav1.Now()
	terminating := runningPod("terminating", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	terminating.DeletionTimestamp = &deletionTime
	terminating.Finalizers = []string{"k8s-sustain.io/test"}

	pending := runningPod("pending", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	pending.Status.Phase = corev1.PodPending

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminating, pending).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
				}
				return nil
			},
		}).
		Build()

	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{terminating, pending}, recs); err != nil {
		t.Fatalf("ResizePodsInPlace: %v", err)
	}
	if resizeCalled {
		t.Error("expected no /resize call for terminating or non-Running pods")
	}
}

// TestResizePodsInPlace_InfeasibleSkipsNoEviction verifies that a pod
// with status.resize=Infeasible is left alone — Job pods must not be evicted.
// The next scheduled run will inherit the new resources via the webhook.
func TestResizePodsInPlace_InfeasibleSkipsNoEviction(t *testing.T) {
	pod := runningPod("infeasible", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	pod.Status.Resize = corev1.PodResizeStatusInfeasible

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evictionCalled, resizeCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{pod}, recs); err != nil {
		t.Fatalf("ResizePodsInPlace: %v", err)
	}
	if resizeCalled {
		t.Error("did not expect /resize for Infeasible status")
	}
	if evictionCalled {
		t.Error("ResizePodsInPlace must never evict, even on Infeasible")
	}
}

// TestResizePodsInPlace_FeatureGateRejectionDoesNotEvict verifies that when
// the API server rejects the resize because the feature gate is disabled
// (Invalid error), the patcher disables in-place mode for subsequent calls
// but does not evict the current pod.
func TestResizePodsInPlace_FeatureGateRejectionDoesNotEvict(t *testing.T) {
	pod := runningPod("rejected", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), pod.Name, nil)
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{pod}, recs); err != nil {
		t.Fatalf("ResizePodsInPlace: %v", err)
	}
	if evictionCalled {
		t.Error("ResizePodsInPlace must never evict, even on feature-gate rejection")
	}
	if p.InPlace() {
		t.Error("expected in-place mode disabled after Invalid response")
	}
}

// statefulSetPod is a builder for StatefulSet-owned pods (e.g. web-0, web-1).
// The OwnerReference points to a StatefulSet so the recycle loop recognizes
// the slice as StatefulSet-owned and sorts by ordinal.
func statefulSetPod(name string, requests corev1.ResourceList) *corev1.Pod {
	p := runningPod(name, requests)
	p.UID = types.UID("uid-" + name)
	controller := true
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       "web",
		Controller: &controller,
	}}
	return p
}

// statefulSetEvictionInterceptor mimics the StatefulSet controller: when a
// pod is evicted it is deleted, then immediately recreated with the SAME name
// but a NEW UID (and freshly applied recommendations), modelling what kubelet
// + the StatefulSet controller actually do in production. Without this the
// fake apiserver lets the evicted name disappear, masking bug 1 where the
// recycle loop only spots replacements by name.
func statefulSetEvictionInterceptor(evictedNames *[]string, recs map[string]ContainerRecommendation, uidCounter *int) interceptor.Funcs {
	var mu sync.Mutex
	return interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
			if sub != "eviction" {
				return nil
			}
			mu.Lock()
			*evictedNames = append(*evictedNames, obj.GetName())
			mu.Unlock()

			// Snapshot the live pod, delete it, then recreate it with the
			// same name + new UID + recommended resources applied.
			key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}
			var live corev1.Pod
			if err := inner.Get(ctx, key, &live); err != nil {
				return err
			}
			if err := inner.Delete(ctx, &live); err != nil {
				return err
			}
			replacement := live.DeepCopy()
			replacement.ResourceVersion = ""
			mu.Lock()
			*uidCounter++
			replacement.UID = types.UID("uid-recycled-" + obj.GetName() + "-" + string(rune('0'+*uidCounter)))
			mu.Unlock()
			ApplyRecommendationsToPodSpec(&replacement.Spec, recs)
			replacement.Status = corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			}
			return inner.Create(ctx, replacement)
		},
	}
}

// TestRecyclePods_StatefulSetEvictsByDescendingOrdinal verifies that pods
// owned by a StatefulSet are evicted in descending ordinal order, AND that
// the post-eviction wait recognises the replacement when the StatefulSet
// controller re-uses the evicted pod's name (different UID).
//
// This is the regression test for bug 1: the wait used to key on pod name
// and would never see a same-named replacement, so every StatefulSet pod
// recycle would only finish via the readyTimeout fallback (and then halt
// the loop).
func TestRecyclePods_StatefulSetEvictsByDescendingOrdinal(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	pods := []*corev1.Pod{
		statefulSetPod("web-0", stale),
		statefulSetPod("web-1", stale),
		statefulSetPod("web-2", stale),
	}
	objs := make([]client.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	uidCounter := 0
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(statefulSetEvictionInterceptor(&evicted, recs, &uidCounter)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})

	// Cap wall-clock duration with a tight per-call timeout. Each
	// waitForReplacement call must return promptly because the replacement
	// is already Ready at observation time. If bug 1 regresses, this test
	// would have to wait readyTimeout * 3 — but the deadline below would
	// catch that long before the standard `go test` timeout.
	start := time.Now()
	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("RecyclePods took %s; replacements should be detected immediately (bug 1 regression?)", elapsed)
	}

	want := []string{"web-2", "web-1", "web-0"}
	if len(evicted) != len(want) {
		t.Fatalf("expected %d evictions, got %d (%v)", len(want), len(evicted), evicted)
	}
	for i := range want {
		if evicted[i] != want[i] {
			t.Errorf("eviction order: got %v, want %v", evicted, want)
			break
		}
	}
}

// TestSortPodsForRecycle_DetectsStatefulSetFromAnyPod verifies that the
// recycle order is StatefulSet-ordinal as long as ANY pod in the slice
// carries the StatefulSet ownerRef. Regression for bug 2: the sort used to
// inspect only pods[0]; if that pod's ownerRef was transiently missing the
// slice degraded to alphabetical (web-0, web-1, web-10, web-2) — wrong.
func TestSortPodsForRecycle_DetectsStatefulSetFromAnyPod(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	// pods[0] has NO ownerRef. pods[1..3] carry the StatefulSet ownerRef.
	// The expected order is descending ordinal among the four pods.
	orphan := runningPod("web-2", stale) // no ownerRef
	owned1 := statefulSetPod("web-0", stale)
	owned10 := statefulSetPod("web-10", stale)
	owned1b := statefulSetPod("web-1", stale)

	pods := []*corev1.Pod{orphan, owned1, owned10, owned1b}
	sortPodsForRecycle(pods)

	wantOrder := []string{"web-10", "web-2", "web-1", "web-0"}
	for i, w := range wantOrder {
		if pods[i].Name != w {
			gotNames := make([]string, len(pods))
			for j, p := range pods {
				gotNames[j] = p.Name
			}
			t.Fatalf("sort order: got %v, want %v", gotNames, wantOrder)
		}
	}
}

// TestRecyclePods_InPlaceInvalidFallback_WaitsForReplacement is the
// regression test for bug 3. When /resize returns IsInvalid (feature gate
// off) and the patcher falls back to eviction inside the in-place path, the
// outer recycle loop used to skip the post-eviction wait for that pod. The
// next pod's eviction would also skip the wait (because p.inPlace is now
// false but the loop already passed the in-place branch for the first pod
// before learning that). After the fix, the very first eviction in a
// gate-detection fallback also goes through waitForReplacement.
func TestRecyclePods_InPlaceInvalidFallback_WaitsForReplacement(t *testing.T) {
	stale1 := runningPod("a", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	stale1.UID = types.UID("uid-a")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	listCallsAfterEviction := 0
	var firstEvictAt time.Time
	var firstListAfterEvictAt time.Time

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale1).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "a", nil)
				}
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "a", nil)
			},
			SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub != "eviction" {
					return nil
				}
				evicted = append(evicted, obj.GetName())
				firstEvictAt = time.Now()
				return inner.Delete(ctx, obj)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if len(evicted) > 0 {
					listCallsAfterEviction++
					if firstListAfterEvictAt.IsZero() {
						firstListAfterEvictAt = time.Now()
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	p := New(c, true /* in-place */, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}

	if len(evicted) != 1 || evicted[0] != "a" {
		t.Fatalf("expected pod 'a' evicted exactly once via in-place fallback, got %v", evicted)
	}
	// After the inline eviction in applyInPlaceResize, the outer loop must
	// run a post-eviction List to wait for quiescence. Without the fix the
	// initial list (used to enumerate pods) was the last list and there
	// would be no post-eviction list at all.
	if listCallsAfterEviction == 0 {
		t.Errorf("expected at least one List after eviction (waitForReplacement should run), got 0")
	}
	if !firstEvictAt.IsZero() && !firstListAfterEvictAt.IsZero() && firstListAfterEvictAt.Before(firstEvictAt) {
		t.Errorf("post-eviction list timing looks off: list=%s eviction=%s", firstListAfterEvictAt, firstEvictAt)
	}
	if p.InPlace() {
		t.Error("expected p.inPlace to be flipped off after IsInvalid")
	}
}

// TestRecyclePods_DefaultOrderIsAlphabetical verifies that pods not owned by
// a StatefulSet are evicted in alphabetical order. The order itself is less
// important than determinism — operators reading logs and tests asserting
// behavior both benefit from a stable sequence.
func TestRecyclePods_DefaultOrderIsAlphabetical(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	pods := []*corev1.Pod{
		runningPod("c", stale),
		runningPod("a", stale),
		runningPod("b", stale),
	}
	objs := make([]client.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(evicted) != len(want) {
		t.Fatalf("expected %d evictions, got %d (%v)", len(want), len(evicted), evicted)
	}
	for i := range want {
		if evicted[i] != want[i] {
			t.Errorf("eviction order: got %v, want %v", evicted, want)
			break
		}
	}
}

// TestRecyclePods_CrashLoopBackOffAbortsLoop verifies that the post-eviction
// wait halts the recycle loop as soon as it sees a CrashLoopBackOff in the
// selector. Without this guard, a bad recommendation that causes pods to
// crashloop on the new resources would cascade through every pod in the
// workload before the next reconcile gets a chance to revise the
// recommendation.
func TestRecyclePods_CrashLoopBackOffAbortsLoop(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	pod1 := runningPod("a", stale)
	pod2 := runningPod("b", stale)
	// A pre-existing crashlooping pod in the selector. Could be the
	// already-recycled previous replacement that's failing on the new request.
	crashing := runningPod("crashloop", stale)
	crashing.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff",
		}},
	}}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod1, pod2, crashing).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	// Expect a non-nil error (the abort) and exactly one eviction (the first
	// pod) — the loop must NOT continue to evict pod "b" while a peer is
	// crashlooping.
	err := p.RecyclePods(context.Background(), "default", sel, recs)
	if err == nil {
		t.Fatal("expected error surfacing CrashLoopBackOff abort, got nil")
	}
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Errorf("expected exactly one eviction (pod 'a'), got %v", evicted)
	}
}

// TestRecyclePods_HPAScaleDownDoesNotStallEvictionLoop verifies that the
// post-eviction wait does NOT hang when an external controller (e.g. HPA)
// scales the workload down below the pre-eviction Ready count. The wait
// looks for a quiescent selector (no Pending peers), not for the original
// Ready-count baseline — otherwise an HPA scale-down concurrent with our
// eviction would block the loop until readyTimeout fires.
func TestRecyclePods_HPAScaleDownDoesNotStallEvictionLoop(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	// Mark the surviving peer as Ready so isProgressing returns false. The
	// HPA-scaled-down case: pre-eviction had {a, b}; we evict 'a'; HPA does
	// not provision a replacement; 'b' is the only remaining pod, and it's
	// Ready. The wait must return immediately, not stall waiting for some
	// frozen baseline of 2 Ready peers.
	pod1 := runningPod("a", stale)
	pod2 := runningPod("b", stale)
	pod2.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod1, pod2).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if err := p.RecyclePods(context.Background(), "default", sel, recs); err != nil {
		t.Fatalf("RecyclePods: %v", err)
	}
	// Both pods should be evicted in order — the wait between them must not
	// stall on a baseline that can never be reached after HPA scale-down.
	if len(evicted) != 2 {
		t.Errorf("expected both pods to be evicted (HPA scale-down should not stall the loop), got %v", evicted)
	}
}

// TestRecyclePods_TimesOutWhenReplacementMissing verifies that when the
// workload controller fails to bring a replacement up (here: the interceptor
// accepts the eviction but never deletes the pod, simulating a stuck
// workload), the recycle loop aborts rather than continuing to evict into a
// broken state. Only the first pod should be evicted; subsequent pods are
// spared until the next reconcile cycle can re-evaluate.
func TestRecyclePods_TimesOutWhenReplacementMissing(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	pod1 := runningPod("a", stale)
	pod2 := runningPod("b", stale)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod1, pod2).
		WithInterceptorFuncs(interceptor.Funcs{
			// Accept the eviction but do NOT delete the pod — emulates a
			// workload stuck (e.g. PDB satisfied but kubelet not draining,
			// or scheduler unable to place the replacement).
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = append(evicted, obj.GetName())
				}
				return nil
			},
		}).
		Build()

	p := New(c, false, WithReadyPollInterval(5*time.Millisecond), WithReadyTimeout(50*time.Millisecond))
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	err := p.RecyclePods(context.Background(), "default", sel, recs)
	if err == nil {
		t.Fatal("expected timeout error when replacement never appears, got nil")
	}
	if len(evicted) != 1 {
		t.Errorf("expected exactly one eviction before timeout halted the loop, got %d (%v)", len(evicted), evicted)
	}
}
