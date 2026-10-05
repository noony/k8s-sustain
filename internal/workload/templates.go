package workload

import (
	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodTemplateOf extracts the pod template from any supported workload object,
// returning ok=false for unsupported types.
//
// The returned pointer aliases the input object — do not mutate it.
func PodTemplateOf(obj client.Object) (template *corev1.PodTemplateSpec, ok bool) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template, true
	case *appsv1.StatefulSet:
		return &o.Spec.Template, true
	case *appsv1.DaemonSet:
		return &o.Spec.Template, true
	case *rolloutsv1alpha1.Rollout:
		return &o.Spec.Template, true
	case *batchv1.CronJob:
		return &o.Spec.JobTemplate.Spec.Template, true
	case *batchv1.Job:
		return &o.Spec.Template, true
	}
	return nil, false
}
