package workload

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodTemplateOf extracts the pod template, owner references, and label
// selector from any supported workload object. Returns ok=false for
// unsupported types so callers can keep their per-kind switches but stop
// reinventing the field-plucking.
//
// Argo Rollouts is intentionally not handled here, though the package as a
// whole now does import the rollouts API (kindobjects.go's ObjectForKind
// table, shared by the webhook and the OOM watcher for reading a workload's
// own annotations) — the old "this package must not depend on the rollouts
// API" reason no longer holds. PodTemplateOf itself still excludes *Rollout
// because nothing calls it for one: every current Rollout call site already
// holds the concrete *rolloutsv1alpha1.Rollout and reads Spec.Template and
// Spec.Selector directly (see internal/controller/workload_target.go,
// rolloutToTarget → newTargetFromTemplate), the pattern this doc comment
// originally described as "branch on the concrete type separately". Adding a
// case here would need its own caller to justify it.
//
// Returned slices/pointers alias the input object — do not mutate them.
func PodTemplateOf(obj client.Object) (template *corev1.PodTemplateSpec, ownerRefs []metav1.OwnerReference, selector *metav1.LabelSelector, ok bool) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template, o.OwnerReferences, o.Spec.Selector, true
	case *appsv1.StatefulSet:
		return &o.Spec.Template, o.OwnerReferences, o.Spec.Selector, true
	case *appsv1.DaemonSet:
		return &o.Spec.Template, o.OwnerReferences, o.Spec.Selector, true
	case *batchv1.CronJob:
		// CronJob has no Pod selector; the JobTemplate owns the pod spec.
		return &o.Spec.JobTemplate.Spec.Template, o.OwnerReferences, nil, true
	case *batchv1.Job:
		return &o.Spec.Template, o.OwnerReferences, o.Spec.Selector, true
	}
	return nil, nil, nil, false
}
