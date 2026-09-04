package workload

import (
	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodTemplateOf extracts the pod template and the label selector the patcher
// recycles through from any supported workload object, returning ok=false for
// unsupported types.
//
// CronJob and Job return a nil selector: their pods are enumerated through the
// batch.kubernetes.io/job-name label and are resized in place, never recycled.
//
// Returned pointers alias the input object — do not mutate them.
func PodTemplateOf(obj client.Object) (template *corev1.PodTemplateSpec, selector *metav1.LabelSelector, ok bool) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template, o.Spec.Selector, true
	case *appsv1.StatefulSet:
		return &o.Spec.Template, o.Spec.Selector, true
	case *appsv1.DaemonSet:
		return &o.Spec.Template, o.Spec.Selector, true
	case *rolloutsv1alpha1.Rollout:
		return &o.Spec.Template, o.Spec.Selector, true
	case *batchv1.CronJob:
		return &o.Spec.JobTemplate.Spec.Template, nil, true
	case *batchv1.Job:
		return &o.Spec.Template, nil, true
	}
	return nil, nil, false
}
