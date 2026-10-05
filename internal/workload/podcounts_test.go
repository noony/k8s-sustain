package workload

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

func jobCPUPod(name, cpu string) *corev1.Pod {
	return jobPod(name, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)})
}

func TestApply_PodCounts_AfterEviction(t *testing.T) {
	c, _ := countsTestClient(t, cpuPod("stale", "50m"), cpuPod("fresh", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pods != (PodCounts{Total: 2, Stale: 0}) {
		t.Errorf("counts = %+v, want {2 0}", out.Pods)
	}
}

func TestApply_DryRun_CountsWithoutMutating(t *testing.T) {
	c, evicted := countsTestClient(t, cpuPod("stale", "50m"), cpuPod("fresh", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed != 0 || len(*evicted) != 0 {
		t.Fatalf("dry-run mutated: changed=%d evicted=%v", out.Changed, *evicted)
	}
	if out.Pods != (PodCounts{Total: 2, Stale: 1}) {
		t.Errorf("counts = %+v, want {2 1}", out.Pods)
	}
}

func TestApply_DryRun_DecreaseWithinToleranceIsFresh(t *testing.T) {
	c, _ := countsTestClient(t, cpuPod("a", "200m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("195m")}}
	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{DryRun: true, Tolerance: tol5})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pods != (PodCounts{Total: 1, Stale: 0}) {
		t.Errorf("counts = %+v, want {1 0}", out.Pods)
	}
}

func TestApply_DryRun_SkipsTerminatingAndTerminal(t *testing.T) {
	done := cpuPod("done", "50m")
	done.Status.Phase = corev1.PodSucceeded
	c, _ := countsTestClient(t, cpuPod("live", "50m"), done)
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pods != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", out.Pods)
	}
}

func TestApply_PodCounts_ExcludeNotOwnedPods(t *testing.T) {
	owned := statefulSetPod("web-0", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	other := statefulSetPod("web2-0", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")})
	other.OwnerReferences[0].Name = "web2"
	other.OwnerReferences[0].UID = "other-sts-uid"
	bare := cpuPod("bare-debug", "50m")
	bare.OwnerReferences = nil

	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}

	t.Run("dry-run", func(t *testing.T) {
		c, _ := countsTestClient(t, owned.DeepCopy(), other.DeepCopy(), bare.DeepCopy())
		p := New(c, false, testEvictionOpts()...)
		out, err := p.Apply(context.Background(), statefulSetMember(), recs, ApplySettings{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Pods != (PodCounts{Total: 1, Stale: 1}) {
			t.Errorf("counts = %+v, want {1 1}", out.Pods)
		}
	})

	t.Run("real pass", func(t *testing.T) {
		c, _ := countsTestClient(t, owned.DeepCopy(), other.DeepCopy(), bare.DeepCopy())
		p := New(c, false, testEvictionOpts()...)
		out, err := p.Apply(context.Background(), statefulSetMember(), recs, ApplySettings{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Pods != (PodCounts{Total: 1, Stale: 0}) {
			t.Errorf("counts = %+v, want {1 0}", out.Pods)
		}
	})
}

func TestApply_InPlaceOnly_NoInPlaceSupport_CountsOnly(t *testing.T) {
	c, _ := countsTestClient(t, jobCPUPod("stale", "50m"))
	p := New(c, false)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pods != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", out.Pods)
	}
}

func TestApply_InPlaceOnly_ResizedPodIsFresh(t *testing.T) {
	c, _ := countsTestClient(t, jobCPUPod("stale", "50m"))
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pods != (PodCounts{Total: 1, Stale: 0}) {
		t.Errorf("counts = %+v, want {1 0}", out.Pods)
	}
}

func countsClientWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestApply_InPlaceOnly_DryRun_NeverResizes(t *testing.T) {
	stale := jobCPUPod("stale", "50m")
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
	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed != 0 || resizeCalls != 0 {
		t.Fatalf("dry run resized: changed=%d resizeCalls=%d", out.Changed, resizeCalls)
	}
	var live corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: stale.Namespace, Name: stale.Name}, &live); err != nil {
		t.Fatal(err)
	}
	if q := live.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "50m" {
		t.Errorf("dry run changed the pod spec, cpu = %s", q.String())
	}
	if out.Pods != (PodCounts{Total: 1, Stale: 1}) {
		t.Errorf("counts = %+v, want {1 1}", out.Pods)
	}
}

// Counts are only measured by a pass that went through: a failed one reports
// none, and the controller keeps the member's previous counts.
func TestApply_InPlaceOnly_ErrorReportsNoCounts(t *testing.T) {
	c := countsClientWith(t, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	}, jobCPUPod("stale", "50m"))
	p := New(c, true)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), jobMember(), recs, ApplySettings{})
	if err == nil {
		t.Fatal("expected the resize error to surface")
	}
	if out.Pods != (PodCounts{}) {
		t.Errorf("counts = %+v, want none", out.Pods)
	}
}

func TestApply_EvictionErrorReportsNoCounts(t *testing.T) {
	c := countsClientWith(t, interceptor.Funcs{
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	}, cpuPod("stale", "50m"))
	p := New(c, false, testEvictionOpts()...)
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
	out, err := p.Apply(context.Background(), testMember(), recs, ApplySettings{})
	if err == nil {
		t.Fatal("expected the eviction error to surface")
	}
	if out.Pods != (PodCounts{}) {
		t.Errorf("counts = %+v, want none", out.Pods)
	}
}
