package workload

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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

// Sidecars run for the pod's lifetime, so drift in one must trigger a recycle.
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

// A recommendation that changes only a limit must still mark the pod stale, or
// eviction-mode reconcile skips it and the stale limit persists forever.
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

// Drift in a classic init container must NOT recycle: it has already exited by
// the time the pod is Running, and the webhook injects on the next creation.
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

// The sidecar-only path skips classic init containers, which have already
// exited, while updating restartable ones.
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

func testSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
}

// testMember is the DaemonSet every runningPod belongs to. DaemonSet pods carry
// a direct controller ownerRef, so ownership needs no ReplicaSet read.
func testMember() Member {
	return Member{Kind: "DaemonSet", Object: &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test", UID: "test-uid"},
		Spec:       appsv1.DaemonSetSpec{Selector: testSelector()},
	}}
}

// runningPod is a small builder for pods of testMember. The pod is Running and
// has PodReady=True so the eviction loop's post-eviction wait sees the workload
// as quiescent — overriding the Conditions in a specific test lets it simulate
// a Pending replacement or a NotReady peer.
func runningPod(name string, requests corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			UID:       types.UID("uid-" + name),
			Labels:    map[string]string{"app": "test"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: "test", UID: "test-uid", Controller: ptr.To(true),
			}},
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

func jobMember() Member {
	return Member{Kind: "Job", Object: &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch", UID: "batch-uid"},
	}}
}

// jobPod is a runningPod of the Job jobMember names instead of testMember.
func jobPod(name string, requests corev1.ResourceList) *corev1.Pod {
	p := runningPod(name, requests)
	p.Labels[batchv1.JobNameLabel] = "batch"
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: "batch", UID: "batch-uid", Controller: ptr.To(true),
	}}
	return p
}

// On a non-in-place cluster stale pods are evicted; pods already at target are
// left alone.
func TestApply_Eviction_HappyPath(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected only 'stale' evicted, got %v", evicted)
	}
}

// The returned count drives the caller's ResourcesUpdated event: it must only
// count pods actually resized or evicted, never pods already at target.
func TestApply_ReturnsChangedCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inPlace bool
	}{
		{"eviction", false},
		{"inPlace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			p := New(c, tc.inPlace, testEvictionOpts()...)
			recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

			out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if out.Changed != 1 {
				t.Errorf("changed count = %d, want 1 (only the stale pod)", out.Changed)
			}
		})
	}
}

// Terminating and terminal pods are skipped, but Pending ones stay eligible: a
// pod stuck Pending on an oversized request is exactly what the webhook should
// re-inject a smaller recommendation for.
func TestApply_SkipsTerminatingAndTerminal(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "pending" {
		t.Errorf("expected only the Pending pod to be evicted, got %v", evicted)
	}
}

// A 429 from the Eviction API (PDB blocking) is a no-op, not an error, so the
// next reconcile retries.
func TestApply_Eviction_PDBBlocked_ReturnsNil(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Errorf("expected nil on PDB block, got %v", err)
	}
}

func TestApply_Eviction_NotFound_ReturnsNil(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Errorf("expected nil on NotFound, got %v", err)
	}
}

func TestApply_InPlace_HappyPath(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize subresource patch to be called")
	}
	if evictionCalled {
		t.Error("did not expect eviction in happy path")
	}
}

// withResizePendingCondition stamps the PodResizePending condition (k8s ≥
// 1.33) on a pod.
func withResizePendingCondition(pod *corev1.Pod, reason string) *corev1.Pod {
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodResizePending,
		Status: corev1.ConditionTrue,
		Reason: reason,
	})
	return pod
}

// withResizeErrorCondition stamps the PodResizeInProgress condition with
// reason Error — the kubelet's report (≥ 1.34) that actuating an accepted
// resize failed.
func withResizeErrorCondition(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodResizeInProgress,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonError,
	})
	return pod
}

// An Infeasible verdict on a spec that already carries the target means the
// apiserver accepted the resize but it cannot land on the node. The pod is
// evicted even though the spec looks fresh — the spec lies about what is
// actually allocated.
func TestApply_InPlace_InfeasibleConditionFallsBackToEviction(t *testing.T) {
	stale := withResizePendingCondition(
		runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
		corev1.PodReasonInfeasible,
	)

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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled {
		t.Error("did not expect /resize when PodResizePending=Infeasible and spec is at target")
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected eviction of 'stale', got %v", evicted)
	}
}

// The kubelet does not retry an errored resize on its own, so without this
// eviction the pod runs on its old allocation forever while the spec claims the
// target.
func TestApply_InPlace_ErroredResizeFallsBackToEviction(t *testing.T) {
	stale := withResizeErrorCondition(
		runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
	)

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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled {
		t.Error("did not expect /resize when the staged resize already errored")
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected eviction of 'stale', got %v", evicted)
	}
}

// An Infeasible verdict for an OLD target must not block a NEW recommendation:
// the kubelet re-evaluates a resubmitted resize, and a lower request may fit.
func TestApply_InPlace_InfeasibleWithNewTargetRetriesResize(t *testing.T) {
	// Spec carries 400m (the old, infeasible target); the new recommendation
	// is 200m, which the node may well be able to satisfy.
	stale := withResizePendingCondition(
		runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")}),
		corev1.PodReasonInfeasible,
	)

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

	p := New(c, true, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize with the NEW target despite stale Infeasible verdict")
	}
	if evictionCalled {
		t.Error("did not expect eviction when a new, untried target exists")
	}
}

// NotFound from /resize means the pod went away between List and patch: no
// direct pod patch, no eviction, no error.
func TestApply_InPlace_ResizeNotFoundSkips(t *testing.T) {
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
					return apierrors.NewNotFound(corev1.Resource("pods"), "stale")
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize attempt")
	}
	if podPatchCalled {
		t.Error("did not expect a direct pod patch — that fallback was removed with pre-1.33 support")
	}
	if evictionCalled {
		t.Error("did not expect eviction when the pod is gone")
	}
}

// If /resize exists, InPlacePodVerticalScaling is on by definition, so Invalid
// means THIS resize is invalid for THIS pod (QoS class change, memory-limit
// decrease under a NotRequired policy). The pod is evicted, but in-place mode
// must stay enabled: every other pod still gets its own /resize try.
func TestApply_InPlace_ResizeInvalidFallsBackToEviction(t *testing.T) {
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
				// A direct pod patch must never be attempted; resizes go
				// through the /resize subresource only.
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 2 {
		t.Errorf("expected both pods to be evicted after IsInvalid; got %v", evicted)
	}
	// Invalid on /resize is a per-pod verdict: the second pod must get its
	// own /resize attempt rather than being demoted to eviction up front.
	if resizeCalls != 2 {
		t.Errorf("expected 2 /resize calls (per-pod Invalid must not disable in-place), got %d", resizeCalls)
	}
	if !p.InPlace() {
		t.Error("per-pod Invalid on /resize must NOT flip p.inPlace off")
	}
}

// A sidecar resize failure (older cluster, gate disabled for sidecars) must not
// fail the reconcile: regular containers are still patched in place.
func TestApply_InPlace_SidecarResizeRejected_BestEffort(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "stale-with-sidecar",
			Labels:    map[string]string{"app": "test"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: "test", UID: "test-uid", Controller: ptr.To(true),
			}},
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
	var firstPatchBody string
	resizeCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, patch client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub != "resize" {
					return nil
				}
				resizeCalls++
				switch resizeCalls {
				case 1:
					// First call = regular containers. Succeed, and capture the
					// patch body so the test can assert the sidecar changes did
					// not leak into it (they must stay in the second, isolated
					// call — otherwise a cluster rejecting sidecar resize would
					// 422 the regular-container patch too).
					regularResizeOK = true
					if data, err := patch.Data(obj); err == nil {
						firstPatchBody = string(data)
					}
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
	recs := map[string]ContainerRecommendation{
		"app":     {CPURequest: qtyp("200m")},
		"sidecar": {CPURequest: qtyp("100m")},
	}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
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
	if firstPatchBody == "" || strings.Contains(firstPatchBody, "initContainers") {
		t.Errorf("regular-container /resize patch must not carry initContainer changes, got body: %q", firstPatchBody)
	}
}

// A deferred resize on a spec already at target is left alone: the kubelet
// applies it when conditions allow.
func TestApply_InPlace_DeferredIsNoOp(t *testing.T) {
	// Spec already at the recommended 200m: the resize was accepted but the
	// kubelet is waiting for room to apply it.
	stale := withResizePendingCondition(
		runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
		corev1.PodReasonDeferred,
	)

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

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled || evictionCalled {
		t.Errorf("expected no-op when resize Deferred (resize=%v, eviction=%v)", resizeCalled, evictionCalled)
	}
}

// A Deferred verdict for an OLD target must not block a NEW recommendation: the
// kubelet re-evaluates the deferred resize against the new values.
func TestApply_InPlace_DeferredWithNewTargetPatches(t *testing.T) {
	stale := withResizePendingCondition(
		runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")}),
		corev1.PodReasonDeferred,
	)

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

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize with the NEW target despite Deferred verdict on the old one")
	}
	if evictionCalled {
		t.Error("did not expect eviction for a Deferred resize")
	}
}

// applyRecToContainer compares before setting, so a pod already at target must
// submit no patch at all rather than an empty one every cycle.
func TestApply_InPlace_AlreadyAtTarget_NoPatch(t *testing.T) {
	fresh := runningPod("fresh", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resizeCalled, podPatchCalled, evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fresh).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled || podPatchCalled || evictionCalled {
		t.Errorf("expected zero patches for pod already at target (resize=%v, patch=%v, eviction=%v)",
			resizeCalled, podPatchCalled, evictionCalled)
	}
}

// A pod deleted between List and patch is a no-op: NotFound must not surface as
// a reconcile error, trigger an eviction, or disable in-place mode.
func TestApply_InPlace_PodGoneDuringResize_NoError(t *testing.T) {
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					return apierrors.NewNotFound(corev1.Resource("pods"), "stale")
				}
				return nil
			},
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewNotFound(corev1.Resource("pods"), "stale")
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

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("expected NotFound on a vanished pod to be a no-op, got: %v", err)
	}
	if evictionCalled {
		t.Error("did not expect eviction for a pod that no longer exists")
	}
	if !p.InPlace() {
		t.Error("NotFound must not disable in-place mode")
	}
}

func TestApply_InPlaceOnly_HappyPath(t *testing.T) {
	stale := jobPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled {
		t.Error("expected /resize subresource patch to be called")
	}
	if evictionCalled {
		t.Error("a Job pod must never be evicted")
	}
	if out.Changed != 1 {
		t.Errorf("resized count = %d, want 1", out.Changed)
	}
}

// The returned count must reflect resizes the API server accepted, or the
// caller reports a ResourcesUpdated event for a resize that never happened.
func TestApply_InPlaceOnly_ReturnsAppliedCount(t *testing.T) {
	ok := jobPod("ok", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	rejected := jobPod("rejected", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ok, rejected).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" && obj.GetName() == "rejected" {
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "rejected", nil)
				}
				return nil
			},
		}).
		Build()

	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Changed != 1 {
		t.Errorf("resized count = %d, want 1 (the Invalid-rejected pod must not be counted)", out.Changed)
	}
}

// A Job pod whose resize errored during actuation is left alone — never
// evicted, never counted. The next run inherits the resources via the webhook.
func TestApply_InPlaceOnly_ErroredResizeSkipsNoEviction(t *testing.T) {
	pod := withResizeErrorCondition(
		jobPod("errored", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
	)

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled || evictionCalled || out.Changed != 0 {
		t.Errorf("expected errored Job pod left alone (resize=%v, eviction=%v, resized=%d)", resizeCalled, evictionCalled, out.Changed)
	}
}

// With inPlace=false there is no eviction fallback: Job pods must finish on
// their existing resources, and the next run inherits new ones via the webhook.
func TestApply_InPlaceOnly_NoOpWhenInPlaceDisabled(t *testing.T) {
	stale := jobPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if anyCall || out.Changed != 0 {
		t.Errorf("a Job pod must be left alone (no API calls, count 0) when inPlace is disabled; resized=%d", out.Changed)
	}
}

func TestApply_InPlaceOnly_SkipsTerminatingAndNonRunning(t *testing.T) {
	deletionTime := metav1.Now()
	terminating := jobPod("terminating", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	terminating.DeletionTimestamp = &deletionTime
	terminating.Finalizers = []string{"k8s-sustain.io/test"}

	pending := jobPod("pending", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled || out.Changed != 0 {
		t.Errorf("expected no /resize call and count 0 for terminating or non-Running pods; resized=%d", out.Changed)
	}
}

// Job pods are never evicted, so an Infeasible resize on a spec already at
// target is left alone; the next run inherits the resources via the webhook.
func TestApply_InPlaceOnly_InfeasibleSkipsNoEviction(t *testing.T) {
	pod := withResizePendingCondition(
		jobPod("infeasible", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
		corev1.PodReasonInfeasible,
	)

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled || out.Changed != 0 {
		t.Errorf("did not expect /resize (or a non-zero count) for Infeasible status; resized=%d", out.Changed)
	}
	if evictionCalled {
		t.Error("a Job pod must never be evicted, even on Infeasible")
	}
}

// An Infeasible verdict on an old target must not stop a NEW recommendation
// from being submitted — the kubelet re-evaluates the new values.
func TestApply_InPlaceOnly_InfeasibleWithNewTargetRetries(t *testing.T) {
	pod := withResizePendingCondition(
		jobPod("infeasible", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")}),
		corev1.PodReasonInfeasible,
	)

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !resizeCalled || out.Changed != 1 {
		t.Errorf("expected /resize with the NEW target despite stale Infeasible verdict; resized=%d", out.Changed)
	}
	if evictionCalled {
		t.Error("a Job pod must never be evicted")
	}
}

// An Invalid response is a per-pod verdict: skip that pod without evicting it,
// and without disabling in-place mode for the others.
func TestApply_InPlaceOnly_PerPodInvalidSkipsWithoutDisabling(t *testing.T) {
	pod := jobPod("rejected", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

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

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if evictionCalled {
		t.Error("a Job pod must never be evicted, even on per-pod Invalid")
	}
	if out.Changed != 0 {
		t.Errorf("Invalid-rejected pod must not be counted as resized; resized=%d", out.Changed)
	}
	if !p.InPlace() {
		t.Error("per-pod Invalid on /resize must NOT disable in-place mode")
	}
}

// NotFound from /resize (pod gone between List and patch) skips without
// eviction, is not counted as resized, and attempts no direct pod patch.
func TestApply_InPlaceOnly_PodGoneSkips(t *testing.T) {
	pod := jobPod("gone", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var podPatchCalled, evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					return apierrors.NewNotFound(corev1.Resource("pods"), pod.Name)
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if evictionCalled || podPatchCalled || out.Changed != 0 {
		t.Errorf("expected gone pod to be skipped (patch=%v, eviction=%v, resized=%d)", podPatchCalled, evictionCalled, out.Changed)
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
		UID:        "sts-uid",
		Controller: &controller,
	}}
	return p
}

func statefulSetMember() Member {
	return Member{Kind: "StatefulSet", Object: &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "sts-uid"},
		Spec:       appsv1.StatefulSetSpec{Selector: testSelector()},
	}}
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
			applyRecsToPodSpec(&replacement.Spec, recs)
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

// StatefulSet pods evict in descending ordinal order, and the post-eviction
// wait must recognise a replacement that reuses the evicted name under a new
// UID — keyed on name, the wait would never see it and every recycle would
// finish only via the readyTimeout fallback.
func TestApply_StatefulSetEvictsByDescendingOrdinal(t *testing.T) {
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
	target := statefulSetMember()

	// Each waitForReplacement call must return promptly because the replacement
	// is already Ready at observation time. A regression would take
	// readyTimeout * 3, which the deadline below catches early.
	start := time.Now()
	if _, err := p.Apply(context.Background(), target, recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("Apply took %s; replacements should be detected immediately (bug 1 regression?)", elapsed)
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

// Ordinal order must hold as long as ANY pod carries the StatefulSet ownerRef:
// inspecting only pods[0] degrades to alphabetical (web-0, web-1, web-10,
// web-2) whenever that pod's ownerRef is transiently missing.
func TestSortPodsForRecycle_DetectsStatefulSetFromAnyPod(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	// pods[0] has NO ownerRef. pods[1..3] carry the StatefulSet ownerRef.
	// The expected order is descending ordinal among the four pods.
	orphan := runningPod("web-2", stale)
	orphan.OwnerReferences = nil
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

// An eviction that happens inside the in-place path's Invalid fallback must
// still go through the outer loop's post-eviction wait.
func TestApply_InPlaceInvalidFallback_WaitsForReplacement(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
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
	if !p.InPlace() {
		t.Error("per-pod Invalid on /resize must NOT flip p.inPlace off")
	}
}

// Non-StatefulSet pods evict alphabetically. The order matters less than the
// determinism, for operators reading logs and for tests.
func TestApply_DefaultOrderIsAlphabetical(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
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

// A CrashLoopBackOff in the selector halts the loop: otherwise a bad
// recommendation cascades through every pod in the workload before the next
// reconcile can revise it.
func TestApply_CrashLoopBackOffAbortsLoop(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	// Expect a non-nil error (the abort) and exactly one eviction (the first
	// pod) — the loop must NOT continue to evict pod "b" while a peer is
	// crashlooping.
	_, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if err == nil {
		t.Fatal("expected error surfacing CrashLoopBackOff abort, got nil")
	}
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Errorf("expected exactly one eviction (pod 'a'), got %v", evicted)
	}
}

func crashLooping(pod *corev1.Pod) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	return pod
}

// The defect this pins: the post-eviction wait halted pass 1 on the
// crash-looping replacement, but pass 2 evicted the next pod before waiting
// and noticing the same replacement, one more pod per reconcile.
func TestApply_CrashLoopingUpdatedPodHaltsEvictionsAcrossPasses(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(runningPod("a", stale), runningPod("b", stale), runningPod("c", stale)).
		WithInterceptorFuncs(interceptor.Funcs{
			// The replacement is admitted with the recommendation and
			// crash-loops on it.
			SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub != "eviction" {
					return nil
				}
				evicted = append(evicted, obj.GetName())
				if err := inner.Delete(ctx, obj); err != nil {
					return err
				}
				replacement := runningPod(obj.GetName()+"-new", nil)
				applyRecsToPodSpec(&replacement.Spec, recs)
				return inner.Create(ctx, crashLooping(replacement))
			},
		}).
		Build()
	p := New(c, false, testEvictionOpts()...)

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); !errors.Is(err, errCrashLoopBackOff) {
		t.Fatalf("pass 1: err = %v, want the crash-loop halt", err)
	}
	if want := []string{"a"}; !slices.Equal(evicted, want) {
		t.Fatalf("pass 1 evicted %v, want %v", evicted, want)
	}

	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if !errors.Is(err, errCrashLoopBackOff) {
		t.Fatalf("pass 2: err = %v, want the crash-loop halt reported again", err)
	}
	if want := []string{"a"}; !slices.Equal(evicted, want) {
		t.Errorf("pass 2 evicted more pods while a-new crash-loops on the recommendation: %v", evicted)
	}
	if out.Changed != 0 {
		t.Errorf("pass 2 changed = %d, want 0", out.Changed)
	}
}

// Only a pod already running the recommendation proves it bad. A stale
// crash-looping pod is evicted like any other: the new numbers may fix it.
func TestApply_CrashLoopingStalePodIsStillEvicted(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(crashLooping(runningPod("a", stale)), runningPod("b", stale)).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if want := []string{"a", "b"}; !slices.Equal(evicted, want) {
		t.Errorf("evicted %v, want %v", evicted, want)
	}
}

// A crash-looping pod on the recommendation halts nothing when no pod is left
// to evict: the member is not Blocked for a crash the pass cannot spread.
func TestApply_CrashLoopingUpdatedPodWithNothingToEvict(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(crashLooping(runningPod("a", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}))).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 0 || out.Pods != (PodCounts{Total: 1, Stale: 0}) {
		t.Errorf("evicted %v, pods %+v; want nothing evicted and the pod counted fresh", evicted, out.Pods)
	}
}

// In place, the halt holds back the eviction fallback; resizes that need no
// eviction are not what spreads a crash across replacements.
func TestApply_InPlace_CrashLoopingUpdatedPodHaltsFallbackEviction(t *testing.T) {
	updated := crashLooping(runningPod("a", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}))
	rejected := runningPod("b", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	funcs := evictionInterceptor(&evicted)
	funcs.SubResourcePatch = func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
		if sub == "resize" {
			return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), obj.GetName(), nil)
		}
		return nil
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(updated, rejected).WithInterceptorFuncs(funcs).Build()
	p := New(c, true, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); !errors.Is(err, errCrashLoopBackOff) {
		t.Fatalf("err = %v, want the crash-loop halt", err)
	}
	if len(evicted) != 0 {
		t.Errorf("evicted %v while a pod on the recommendation crash-loops", evicted)
	}
}

// The wait looks for a quiescent selector, not for the pre-eviction Ready-count
// baseline: an HPA scale-down concurrent with an eviction would otherwise block
// the loop until readyTimeout.
func TestApply_HPAScaleDownDoesNotStallEvictionLoop(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Both pods should be evicted in order — the wait between them must not
	// stall on a baseline that can never be reached after HPA scale-down.
	if len(evicted) != 2 {
		t.Errorf("expected both pods to be evicted (HPA scale-down should not stall the loop), got %v", evicted)
	}
}

// When no replacement comes up the loop must abort rather than evict into a
// broken state: only the first pod goes, the rest wait for the next reconcile.
func TestApply_TimesOutWhenReplacementMissing(t *testing.T) {
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	_, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if err == nil {
		t.Fatal("expected timeout error when replacement never appears, got nil")
	}
	if len(evicted) != 1 {
		t.Errorf("expected exactly one eviction before timeout halted the loop, got %d (%v)", len(evicted), evicted)
	}
}

// replicaSetOwnedPod is a builder for Deployment/Rollout-style pods: the
// controller ownerRef points at an intermediate ReplicaSet, whose own
// controller ownerRef identifies the top-level workload.
func replicaSetOwnedPod(name, rsName string, requests corev1.ResourceList) *corev1.Pod {
	p := runningPod(name, requests)
	p.UID = types.UID("uid-" + name)
	controller := true
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       rsName,
		UID:        types.UID("uid-" + rsName),
		Controller: &controller,
	}}
	return p
}

func deploymentMember(name string, uid types.UID) Member {
	return Member{Kind: "Deployment", Object: &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: uid},
		Spec:       appsv1.DeploymentSpec{Selector: testSelector()},
	}}
}

// replicaSetOwnedBy is a builder for ReplicaSets controlled by a Deployment
// with the given name and UID.
func replicaSetOwnedBy(name, deployName string, deployUID types.UID) *appsv1.ReplicaSet {
	controller := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			UID:       types.UID("uid-" + name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
				UID:        deployUID,
				Controller: &controller,
			}},
		},
	}
}

// The opt-in contract is per-workload, so a bare debug pod with the same labels
// and a pod behind another Deployment's ReplicaSet must be left untouched. The
// ReplicaSet→owner lookup is memoized per pass: one GET per distinct
// ReplicaSet, not per pod.
func TestApply_Eviction_SkipsPodsNotOwnedByTarget(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	owned1 := replicaSetOwnedPod("owned-1", "web-abc", stale)
	owned2 := replicaSetOwnedPod("owned-2", "web-abc", stale)
	foreign := replicaSetOwnedPod("zz-foreign", "other-abc", stale)
	bare := runningPod("zz-bare-debug", stale)
	bare.OwnerReferences = nil

	targetRS := replicaSetOwnedBy("web-abc", "web", "dep-uid")
	otherRS := replicaSetOwnedBy("other-abc", "other", "other-dep-uid")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	rsGets := 0
	funcs := evictionInterceptor(&evicted)
	funcs.Get = func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*appsv1.ReplicaSet); ok {
			rsGets++
		}
		return cl.Get(ctx, key, obj, opts...)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(owned1, owned2, foreign, bare, targetRS, otherRS).
		WithInterceptorFuncs(funcs).
		Build()

	p := New(c, false /* eviction path */, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	target := deploymentMember("web", "dep-uid")

	if _, err := p.Apply(context.Background(), target, recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{"owned-1", "owned-2"}
	if len(evicted) != len(want) || evicted[0] != want[0] || evicted[1] != want[1] {
		t.Errorf("expected only owned pods evicted in order %v, got %v", want, evicted)
	}
	// Two distinct ReplicaSets referenced by three pods: the per-pass memo
	// must keep the GETs at two.
	if rsGets != 2 {
		t.Errorf("expected 2 ReplicaSet GETs (memoized per recycle pass), got %d", rsGets)
	}

	// The bystanders must still exist, untouched.
	for _, name := range []string{"zz-bare-debug", "zz-foreign"} {
		var got corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got); err != nil {
			t.Errorf("bystander pod %s should still exist: %v", name, err)
		}
	}
}

// The same ownership filter on the in-place path: only a pod whose controller
// ownerRef UID matches the target is resized.
func TestApply_InPlace_SkipsPodsNotOwnedByTarget(t *testing.T) {
	stale := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	owned := statefulSetPod("web-0", stale)
	owned.OwnerReferences[0].UID = "sts-uid"
	other := statefulSetPod("web2-0", stale)
	other.OwnerReferences[0].Name = "web2"
	other.OwnerReferences[0].UID = "other-sts-uid"
	bare := runningPod("bare-debug", stale)
	bare.OwnerReferences = nil

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var resized []string
	var evictionCalled bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(owned, other, bare).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resized = append(resized, obj.GetName())
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
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	target := statefulSetMember()

	if _, err := p.Apply(context.Background(), target, recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(resized) != 1 || resized[0] != "web-0" {
		t.Errorf("expected only the owned pod web-0 resized, got %v", resized)
	}
	if evictionCalled {
		t.Error("no pod should be evicted on the in-place path")
	}
}

func TestApply_SuppressesSmallDecrease(t *testing.T) {
	// Pod at 1000m CPU. Rec 995m (-5m) is below the band max(50m,10m)=50m.
	pod := runningPod("p", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	var evicted []string
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).Build()
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("995m")}}

	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{Tolerance: tol5})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("sub-threshold decrease must not evict, got %v", evicted)
	}
	// The controller's suppression metric is fed from this count.
	if want := map[string]int{"cpu": 1}; !maps.Equal(out.Suppressed, want) {
		t.Fatalf("suppressed = %v, want %v", out.Suppressed, want)
	}
}

func TestApply_AppliesIncrease(t *testing.T) {
	pod := runningPod("p", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	var evicted []string
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).Build()
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("1010m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{Tolerance: tol5}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 1 {
		t.Fatalf("increase must recycle, got %v", evicted)
	}
}

// The in-place path honours the downsize tolerance: a sub-threshold decrease
// produces no /resize patch and is counted as suppressed.
func TestApply_InPlaceOnly_SuppressesSmallDecrease(t *testing.T) {
	// Pod at 1000m CPU. Rec 995m (-5m) is below the band max(50m,10m)=50m.
	pod := jobPod("p", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	var resizeCalled bool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					resizeCalled = true
				}
				return nil
			},
		}).Build()
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("995m")}}

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{Tolerance: tol5})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if resizeCalled {
		t.Error("sub-threshold decrease must not trigger an in-place resize")
	}
	if out.Changed != 0 {
		t.Errorf("resized count = %d, want 0", out.Changed)
	}
	if want := map[string]int{"cpu": 1}; !maps.Equal(out.Suppressed, want) {
		t.Fatalf("suppressed = %v, want %v", out.Suppressed, want)
	}
}

// Tolerance is per-dimension: when CPU crosses the threshold but memory does
// not, the patch carries the new CPU request only and memory alone is reported
// suppressed.
func TestApply_InPlaceOnly_MixedRecAppliesCrossingDimensionOnly(t *testing.T) {
	// current CPU 1000m, memory 1Gi (1024Mi).
	// rec CPU 900m: delta 100m >= band max(50m,10m)=50m -> applied.
	// rec memory 1000Mi: delta 24Mi < band max(51Mi,15Mi)=51Mi -> suppressed (kept 1Gi).
	pod := jobPod("p", corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000m"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	var patched *corev1.Pod
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					patched = obj.(*corev1.Pod).DeepCopy()
				}
				return nil
			},
		}).Build()
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("900m"), MemoryRequest: qtyp("1000Mi")}}

	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{Tolerance: tol5})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Changed != 1 {
		t.Fatalf("resized count = %d, want 1 (CPU crosses threshold)", out.Changed)
	}
	if patched == nil {
		t.Fatal("expected a /resize patch carrying the CPU change")
	}
	got := patched.Spec.Containers[0].Resources.Requests
	if got.Cpu().Cmp(resource.MustParse("900m")) != 0 {
		t.Errorf("CPU request = %s, want 900m (the crossing dimension is applied)", got.Cpu().String())
	}
	if got.Memory().Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory request = %s, want 1Gi unchanged (sub-threshold decrease suppressed)", got.Memory().String())
	}
	if want := map[string]int{"memory": 1}; !maps.Equal(out.Suppressed, want) {
		t.Fatalf("suppressed = %v, want %v (memory only)", out.Suppressed, want)
	}
}

// withSafeToEvictAnnotation stamps the cluster-autoscaler safe-to-evict
// annotation on a pod.
func withSafeToEvictAnnotation(pod *corev1.Pod, value string) *corev1.Pod {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[SafeToEvictAnnotation] = value
	return pod
}

func TestApply_SafeToEvictFalseBlocksEviction(t *testing.T) {
	blocked := withSafeToEvictAnnotation(
		runningPod("blocked", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}), "false")
	stale := runningPod("stale", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(blocked, stale).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected only 'stale' evicted (annotated pod skipped), got %v", evicted)
	}
}

func TestApply_SafeToEvictIgnoredWhenOptionSet(t *testing.T) {
	blocked := withSafeToEvictAnnotation(
		runningPod("blocked", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}), "false")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(blocked).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{IgnoreSafeToEvict: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "blocked" {
		t.Errorf("expected 'blocked' evicted with ignore option, got %v", evicted)
	}
}

// Only the literal value "false" blocks eviction, matching cluster-autoscaler.
func TestApply_SafeToEvictNonFalseValueDoesNotBlock(t *testing.T) {
	safeTrue := withSafeToEvictAnnotation(
		runningPod("safe-true", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}), "true")
	garbage := withSafeToEvictAnnotation(
		runningPod("garbage", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}), "no")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(safeTrue, garbage).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 2 {
		t.Errorf("expected both pods evicted (annotation not 'false'), got %v", evicted)
	}
}

// The safe-to-evict gate covers the in-place path's eviction fallback too.
func TestApply_InPlace_InfeasibleFallbackHonorsSafeToEvict(t *testing.T) {
	blocked := withSafeToEvictAnnotation(
		withResizePendingCondition(
			runningPod("blocked", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}),
			corev1.PodReasonInfeasible,
		), "false")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	var evicted []string
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(blocked).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).
		Build()

	p := New(c, true /* in-place */, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	if _, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(evicted) != 0 {
		t.Errorf("expected no eviction for annotated pod, got %v", evicted)
	}
}

func applyRecsToPodSpec(spec *corev1.PodSpec, recs map[string]ContainerRecommendation) {
	for i := range spec.Containers {
		applyRecToContainer(&spec.Containers[i], recs[spec.Containers[i].Name])
	}
	for i := range spec.InitContainers {
		applyRecToContainer(&spec.InitContainers[i], recs[spec.InitContainers[i].Name])
	}
}
