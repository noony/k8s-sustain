package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestListJobTargets_SkipsCronJobOwnedAndTerminal(t *testing.T) {
	standalone := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "standalone", UID: "s-uid"},
	}
	cronOwned := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "cron-owned",
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), Kind: "CronJob", Name: "nightly"}},
		},
	}
	terminal := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "done", UID: "d-uid"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	r := makeReconciler(t, standalone, cronOwned, terminal)

	got, err := r.listJobTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("listJobTargets: %v", err)
	}
	if len(got) != 1 || got[0].Name != "standalone" {
		names := make([]string, len(got))
		for i, x := range got {
			names[i] = x.Name
		}
		t.Errorf("expected only [standalone], got %v", names)
	}
}

func TestJobToTarget_FromPodTemplate(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch-1", UID: "job-uid"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
			},
		},
	}
	got := jobToTarget(job)
	if got.Kind != "Job" {
		t.Errorf("Kind = %q, want Job", got.Kind)
	}
	if got.Name != "batch-1" || got.Namespace != "default" {
		t.Errorf("Name/Namespace = %q/%q, want batch-1/default", got.Name, got.Namespace)
	}
	if got.PolicyName != "p" {
		t.Errorf("PolicyName = %q, want p", got.PolicyName)
	}
	if len(got.Containers) != 1 || got.Containers[0].Name != "worker" {
		t.Errorf("Containers = %v, want [worker]", got.Containers)
	}
	if got.Selector != nil {
		t.Errorf("Selector = %v, want nil (job path enumerates by job-name label)", got.Selector)
	}
}

// helper: a standalone Job plus one running pod owned by it.
func standaloneJobWithPod() (*batchv1.Job, *corev1.Pod) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch-1", UID: "job-uid"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "batch-1-abc",
			Labels:          map[string]string{jobPodNameLabel: "batch-1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "job-uid", Kind: "Job", Name: "batch-1"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "worker",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return job, pod
}

func TestResizeJobPods_ResizesInPlaceNeverEvicts(t *testing.T) {
	job, pod := standaloneJobWithPod()

	var jobPatched, podResized, evicted bool
	r := makeReconciler(t, job, pod)
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithObjects(job, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					jobPatched = true
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					if _, ok := obj.(*corev1.Pod); ok {
						podResized = true
					}
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = true
				}
				return nil
			},
		}).
		Build()
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := jobToTarget(job)
	recs := map[string]workload.ContainerRecommendation{
		"worker": {CPURequest: qty("250m")},
	}
	resized, err := r.resizeJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeJobPods: %v", err)
	}
	if jobPatched {
		t.Error("controller must not patch the Job spec")
	}
	if !podResized {
		t.Error("expected /resize subresource patch on the running job pod")
	}
	if evicted {
		t.Error("controller must never evict standalone job pods")
	}
	if resized != 1 {
		t.Errorf("resized count = %d, want 1", resized)
	}
}

func TestResizeJobPods_ZeroWhenNoInPlaceSupport(t *testing.T) {
	job, pod := standaloneJobWithPod()
	r := makeReconciler(t, job, pod)
	r.patcher = workload.New(r.Client, false /* no in-place */)

	target := jobToTarget(job)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("250m")}}
	resized, err := r.resizeJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeJobPods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count without in-place support = %d, want 0", resized)
	}
}

func TestResizeJobPods_TerminalJobResizesNothing(t *testing.T) {
	job, pod := standaloneJobWithPod()
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	r := makeReconciler(t, job, pod)
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := jobToTarget(job)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("250m")}}
	resized, err := r.resizeJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeJobPods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count for terminal job = %d, want 0", resized)
	}
}

func TestResizeJobPods_PerPodInvalidNotCounted(t *testing.T) {
	job, pod := standaloneJobWithPod()
	r := makeReconciler(t, job, pod)
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithObjects(job, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), obj.GetName(), nil)
				}
				return nil
			},
		}).
		Build()
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := jobToTarget(job)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("250m")}}
	resized, err := r.resizeJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeJobPods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count = %d, want 0 (the pod's resize was rejected)", resized)
	}
}

func TestResizeJobPods_NoRunningPodsResizesNothing(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch-1", UID: "job-uid"}}
	r := makeReconciler(t, job) // job present, no pods
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := jobToTarget(job)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("250m")}}
	resized, err := r.resizeJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeJobPods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized = %d, want 0 (no running pods)", resized)
	}
}

func TestReconcileWorkload_JobResizesRunningPod(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "batch-1", UID: "job-uid"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
					},
				}}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "batch-1-abc",
			Labels:          map[string]string{jobPodNameLabel: "batch-1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "job-uid", Kind: "Job", Name: "batch-1"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := reconcilerWithProm(t, server, true /* in-place */, pod)

	var podResized, evicted bool
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					if _, ok := obj.(*corev1.Pod); ok {
						podResized = true
					}
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evicted = true
				}
				return nil
			},
		}).
		Build()
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := jobToTarget(job)
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(&target)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if !podResized {
		t.Error("expected the running job pod to be resized in place")
	}
	if evicted {
		t.Error("controller must never evict a standalone job pod")
	}
}
