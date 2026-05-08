package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// TestPatchCronJobTemplate_UpdatesContainersAndPersists verifies that a
// CronJob target's JobTemplate is mutated in place and the change is
// persisted back through the client.
func TestPatchCronJobTemplate_UpdatesContainersAndPersists(t *testing.T) {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly"},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: "app",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("64Mi"),
									},
								},
							}},
						},
					},
				},
			},
		},
	}
	r := makeReconciler(t, cj)
	target := cronJobToTarget(cj)
	recs := map[string]workload.ContainerRecommendation{
		"app": {
			CPURequest:    qty("250m"),
			MemoryRequest: qty("128Mi"),
		},
	}

	if err := r.patchCronJobTemplate(context.Background(), &target, recs); err != nil {
		t.Fatalf("patch: %v", err)
	}

	var got batchv1.CronJob
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "nightly"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	c := got.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if cpu := c.Resources.Requests[corev1.ResourceCPU]; cpu.MilliValue() != 250 {
		t.Errorf("cpu request not patched: %v", &cpu)
	}
	if mem := c.Resources.Requests[corev1.ResourceMemory]; mem.Value() != 128*1024*1024 {
		t.Errorf("memory request not patched: %v", &mem)
	}
}

// TestPatchCronJobTemplate_PreservesUnrelatedFields verifies the patch
// targets only the JobTemplate containers and leaves the schedule, history
// limits, and other fields intact.
func TestPatchCronJobTemplate_PreservesUnrelatedFields(t *testing.T) {
	successLimit := int32(7)
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nightly"},
		Spec: batchv1.CronJobSpec{
			Schedule:                   "0 3 * * *",
			SuccessfulJobsHistoryLimit: &successLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "app"}},
						},
					},
				},
			},
		},
	}
	r := makeReconciler(t, cj)
	target := cronJobToTarget(cj)
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qty("250m")},
	}

	if err := r.patchCronJobTemplate(context.Background(), &target, recs); err != nil {
		t.Fatalf("patch: %v", err)
	}

	var got batchv1.CronJob
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "nightly"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Schedule != "0 3 * * *" {
		t.Errorf("schedule clobbered: %q", got.Spec.Schedule)
	}
	if got.Spec.SuccessfulJobsHistoryLimit == nil || *got.Spec.SuccessfulJobsHistoryLimit != 7 {
		t.Errorf("successfulJobsHistoryLimit clobbered: %v", got.Spec.SuccessfulJobsHistoryLimit)
	}
}
