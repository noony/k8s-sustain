package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestIsOwnedBy_ControllerRefMatch(t *testing.T) {
	uid := types.UID("cj-uid")
	tests := []struct {
		name string
		refs []metav1.OwnerReference
		want bool
	}{
		{"empty", nil, false},
		{"different uid", []metav1.OwnerReference{{Controller: ptr.To(true), UID: "other"}}, false},
		{"matching uid but not controller", []metav1.OwnerReference{{Controller: ptr.To(false), UID: uid}}, false},
		{"matching uid with controller", []metav1.OwnerReference{{Controller: ptr.To(true), UID: uid}}, true},
		{"controller nil", []metav1.OwnerReference{{UID: uid}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := workload.IsOwnedBy(tc.refs, uid); got != tc.want {
				t.Errorf("isOwnedBy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJobIsTerminal_TrueOnCompleteOrFailed(t *testing.T) {
	tests := []struct {
		name       string
		conditions []batchv1.JobCondition
		want       bool
	}{
		{"none", nil, false},
		{"complete-true", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}, true},
		{"failed-true", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}, true},
		{"complete-false", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}}, false},
		{"suspended", []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conditions}}
			if got := jobIsTerminal(job); got != tc.want {
				t.Errorf("jobIsTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListActiveJobsForCronJob_FiltersOwnerAndState(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly", UID: "cj-uid"},
	}
	owned := func(name string, terminal bool) *batchv1.Job {
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "default",
				Name:            name,
				OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "cj-uid", Kind: "CronJob", Name: "nightly"}},
			},
		}
		if terminal {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return j
	}
	other := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "unrelated",
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "other-uid", Kind: "CronJob"}},
		},
	}
	r := makeReconciler(t, cj, owned("active", false), owned("done", true), other)

	got, err := r.listActiveJobsForCronJob(context.Background(), cj)
	if err != nil {
		t.Fatalf("listActiveJobsForCronJob: %v", err)
	}
	if len(got) != 1 || got[0].Name != "active" {
		names := make([]string, len(got))
		for i, j := range got {
			names[i] = j.Name
		}
		t.Errorf("expected only [active], got %v", names)
	}
}

// A label-matching pod without a controller ownerRef to the Job (e.g. a bare
// pod carrying a forged label) must be filtered out.
func TestListPodsForJob_LabelSelector(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "j1", UID: "j1-uid"}}
	matching := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "p1",
			Labels:          map[string]string{jobPodNameLabel: "j1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "j1-uid", Kind: "Job", Name: "j1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	wrongJob := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "p2",
			Labels:    map[string]string{jobPodNameLabel: "different"},
		},
	}
	unowned := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "p3",
			Labels:    map[string]string{jobPodNameLabel: "j1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := makeReconciler(t, job, matching, wrongJob, unowned)

	got, err := r.listPodsForJob(context.Background(), job)
	if err != nil {
		t.Fatalf("listPodsForJob: %v", err)
	}
	if len(got) != 1 || got[0].Name != "p1" {
		t.Errorf("expected only p1, got %v", got)
	}
}

// The CronJob spec must be untouched after reconcile: in-place pod resize
// handles the running pods, the webhook handles new runs.
func TestResizeCronJobPods_NeverPatchesCronJob(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly", UID: "cj-uid"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
						},
						Spec: corev1.PodSpec{Containers: []corev1.Container{{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						}}},
					},
				},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1",
			UID:             "job-uid",
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "cj-uid", Kind: "CronJob"}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1-abc",
			Labels:          map[string]string{jobPodNameLabel: "nightly-1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "job-uid", Kind: "Job", Name: "nightly-1"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	var cronjobPatched, podResized, evicted bool
	r := makeReconciler(t, cj, job, pod)
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithObjects(cj, job, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*batchv1.CronJob); ok {
					cronjobPatched = true
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

	target := cronJobToTarget(cj)
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qty("250m"), MemoryRequest: qty("128Mi")},
	}
	resized, err := r.resizeCronJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeCronJobPods: %v", err)
	}

	if cronjobPatched {
		t.Error("controller must not patch the CronJob spec")
	}
	if !podResized {
		t.Error("expected /resize subresource patch on the running cronjob pod")
	}
	if evicted {
		t.Error("controller must never evict cronjob pods")
	}
	if resized != 1 {
		t.Errorf("resized count = %d, want 1", resized)
	}
}

// The caller relies on a zero count to suppress the misleading ResourcesUpdated
// event, since the JobTemplate is never mutated.
func TestResizeCronJobPods_ReturnsZeroWhenNothingResized(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly", UID: "cj-uid"},
		Spec:       batchv1.CronJobSpec{Schedule: "* * * * *"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1",
			UID:             "job-uid",
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "cj-uid", Kind: "CronJob"}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1-abc",
			Labels:          map[string]string{jobPodNameLabel: "nightly-1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "job-uid", Kind: "Job", Name: "nightly-1"}},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qty("250m")},
	}

	// No active jobs at all.
	r := makeReconciler(t, cj)
	r.patcher = workload.New(r.Client, true /* in-place */)
	target := cronJobToTarget(cj)
	resized, err := r.resizeCronJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeCronJobPods (no jobs): %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count with no active jobs = %d, want 0", resized)
	}

	// Active running pod, but the cluster doesn't support in-place resize.
	r = makeReconciler(t, cj, job, pod)
	r.patcher = workload.New(r.Client, false /* no in-place */)
	resized, err = r.resizeCronJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeCronJobPods (no in-place): %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count without in-place support = %d, want 0", resized)
	}
}

// A /resize rejected as Invalid (e.g. a QoS class change) must not be counted,
// or the caller emits a ResourcesUpdated event for a resize that never happened.
func TestResizeCronJobPods_PerPodInvalidNotCounted(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly", UID: "cj-uid"},
		Spec:       batchv1.CronJobSpec{Schedule: "* * * * *"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1",
			UID:             "job-uid",
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "cj-uid", Kind: "CronJob"}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "nightly-1-abc",
			Labels:          map[string]string{jobPodNameLabel: "nightly-1"},
			OwnerReferences: []metav1.OwnerReference{{Controller: ptr.To(true), UID: "job-uid", Kind: "Job", Name: "nightly-1"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := makeReconciler(t, cj, job, pod)
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithObjects(cj, job, pod).
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

	target := cronJobToTarget(cj)
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qty("250m")},
	}
	resized, err := r.resizeCronJobPods(context.Background(), &target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeCronJobPods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized count = %d, want 0 (the pod's resize was rejected)", resized)
	}
}
