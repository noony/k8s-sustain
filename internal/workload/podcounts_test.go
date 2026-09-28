package workload

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func countsTestClient(t *testing.T, objs ...client.Object) (client.Client, *[]string) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	var evicted []string
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(evictionInterceptor(&evicted)).Build()
	return c, &evicted
}

func cpuPod(name, cpu string) *corev1.Pod {
	return runningPod(name, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)})
}

func appSelector(t *testing.T) klabels.Selector {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func TestRecyclePods_PodCounts_AfterEviction(t *testing.T) {
	c, _ := countsTestClient(t, cpuPod("stale", "50m"), cpuPod("fresh", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	if _, err := p.RecyclePods(context.Background(), TargetWorkload{}, "default", appSelector(t), recs, WithPodCounts(&got)); err != nil {
		t.Fatal(err)
	}
	if got != (PodCounts{Total: 2, Stale: 0}) {
		t.Errorf("counts = %+v, want {2 0}", got)
	}
}

func TestRecyclePods_DryRun_CountsWithoutMutating(t *testing.T) {
	c, evicted := countsTestClient(t, cpuPod("stale", "50m"), cpuPod("fresh", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	changed, err := p.RecyclePods(context.Background(), TargetWorkload{}, "default", appSelector(t), recs, WithDryRun(), WithPodCounts(&got))
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || len(*evicted) != 0 {
		t.Fatalf("dry-run mutated: changed=%d evicted=%v", changed, *evicted)
	}
	if got != (PodCounts{Total: 2, Stale: 1}) {
		t.Errorf("counts = %+v, want {2 1}", got)
	}
}

func TestRecyclePods_DryRun_DecreaseWithinToleranceIsFresh(t *testing.T) {
	c, _ := countsTestClient(t, cpuPod("a", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("195m")}}
	tol := Tolerance{CPUPercent: 5, CPUFloor: resource.MustParse("10m"), MemPercent: 5, MemFloor: resource.MustParse("15Mi")}
	var got PodCounts
	if _, err := p.RecyclePods(context.Background(), TargetWorkload{}, "default", appSelector(t), recs, WithDryRun(), WithTolerance(tol), WithPodCounts(&got)); err != nil {
		t.Fatal(err)
	}
	if got != (PodCounts{Total: 1, Stale: 0}) {
		t.Errorf("counts = %+v, want {1 0}", got)
	}
}

func TestRecyclePods_DryRun_SkipsTerminatingAndTerminal(t *testing.T) {
	done := cpuPod("done", "50m")
	done.Status.Phase = corev1.PodSucceeded
	c, _ := countsTestClient(t, cpuPod("live", "50m"), done)
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	if _, err := p.RecyclePods(context.Background(), TargetWorkload{}, "default", appSelector(t), recs, WithDryRun(), WithPodCounts(&got)); err != nil {
		t.Fatal(err)
	}
	if got != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", got)
	}
}

func TestRecyclePods_PodCounts_ExcludeNotOwnedPods(t *testing.T) {
	owned := statefulSetPod("web-0", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	owned.OwnerReferences[0].UID = "sts-uid"
	other := statefulSetPod("web2-0", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	other.OwnerReferences[0].Name = "web2"
	other.OwnerReferences[0].UID = "other-sts-uid"
	bare := cpuPod("bare-debug", "50m")

	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	target := TargetWorkload{Kind: "StatefulSet", Name: "web", UID: "sts-uid"}

	t.Run("dry-run", func(t *testing.T) {
		c, _ := countsTestClient(t, owned.DeepCopy(), other.DeepCopy(), bare.DeepCopy())
		p := New(c, false, testEvictionOpts()...)
		var got PodCounts
		if _, err := p.RecyclePods(context.Background(), target, "default", appSelector(t), recs, WithDryRun(), WithPodCounts(&got)); err != nil {
			t.Fatal(err)
		}
		if got != (PodCounts{Total: 1, Stale: 1}) {
			t.Errorf("counts = %+v, want {1 1}", got)
		}
	})

	t.Run("real pass", func(t *testing.T) {
		c, _ := countsTestClient(t, owned.DeepCopy(), other.DeepCopy(), bare.DeepCopy())
		p := New(c, false, testEvictionOpts()...)
		var got PodCounts
		if _, err := p.RecyclePods(context.Background(), target, "default", appSelector(t), recs, WithPodCounts(&got)); err != nil {
			t.Fatal(err)
		}
		if got != (PodCounts{Total: 1, Stale: 0}) {
			t.Errorf("counts = %+v, want {1 0}", got)
		}
	})
}

func TestResizePodsInPlace_NoInPlaceSupport_CountsOnly(t *testing.T) {
	stale := cpuPod("stale", "50m")
	c, _ := countsTestClient(t, stale)
	p := New(c, false)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	if _, err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs, WithPodCounts(&got)); err != nil {
		t.Fatal(err)
	}
	if got != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", got)
	}
}

func TestResizePodsInPlace_ResizedPodIsFresh(t *testing.T) {
	stale := cpuPod("stale", "50m")
	c, _ := countsTestClient(t, stale)
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	if _, err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs, WithPodCounts(&got)); err != nil {
		t.Fatal(err)
	}
	if got != (PodCounts{Total: 1, Stale: 0}) {
		t.Errorf("counts = %+v, want {1 0}", got)
	}
}

func countsClientWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestResizePodsInPlace_DryRun_NeverResizes(t *testing.T) {
	stale := cpuPod("stale", "50m")
	var resizeCalls int
	c := countsClientWith(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, inner client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				resizeCalls++
			}
			return inner.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, stale)
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	var got PodCounts
	resized, err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs, WithDryRun(), WithPodCounts(&got))
	if err != nil {
		t.Fatal(err)
	}
	if resized != 0 || resizeCalls != 0 {
		t.Fatalf("dry run resized: resized=%d resizeCalls=%d", resized, resizeCalls)
	}
	var live corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: stale.Namespace, Name: stale.Name}, &live); err != nil {
		t.Fatal(err)
	}
	if q := live.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "50m" {
		t.Errorf("dry run changed the pod spec, cpu = %s", q.String())
	}
	if got != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", got)
	}
}

func TestResizePodsInPlace_ErrorLeavesCountsUntouched(t *testing.T) {
	stale := cpuPod("stale", "50m")
	c := countsClientWith(t, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	}, stale)
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	seeded := PodCounts{Total: 7, Stale: 3}
	got := seeded
	if _, err := p.ResizePodsInPlace(context.Background(), []*corev1.Pod{stale}, recs, WithPodCounts(&got)); err == nil {
		t.Fatal("expected the resize error to surface")
	}
	if got != seeded {
		t.Errorf("counts = %+v, want untouched %+v", got, seeded)
	}
}

func TestRecyclePods_EvictionErrorLeavesCountsUntouched(t *testing.T) {
	c := countsClientWith(t, interceptor.Funcs{
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	}, cpuPod("stale", "50m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	seeded := PodCounts{Total: 7, Stale: 3}
	got := seeded
	if _, err := p.RecyclePods(context.Background(), TargetWorkload{}, "default", appSelector(t), recs, WithPodCounts(&got)); err == nil {
		t.Fatal("expected the eviction error to surface")
	}
	if got != seeded {
		t.Errorf("counts = %+v, want untouched %+v", got, seeded)
	}
}
