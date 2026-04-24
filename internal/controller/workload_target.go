package controller

import (
	"slices"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// workloadTarget is the unit of work for reconciliation. It represents a single
// workload (Deployment, StatefulSet, DaemonSet, Rollout) that matches a Policy.
type workloadTarget struct {
	Kind       string
	Name       string
	Namespace  string
	PolicyName string
	Containers []corev1.Container
	Selector   *metav1.LabelSelector
	Object     client.Object
}

// key returns a unique identifier for this workload target, used as the retry map key.
func (w *workloadTarget) key() string { //nolint:unused // used in Task 5 reconcile rewrite
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

func deploymentToTarget(d *appsv1.Deployment) workloadTarget {
	return workloadTarget{
		Kind:       "Deployment",
		Name:       d.Name,
		Namespace:  d.Namespace,
		PolicyName: d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers: d.Spec.Template.Spec.Containers,
		Selector:   d.Spec.Selector,
		Object:     d,
	}
}

func statefulSetToTarget(s *appsv1.StatefulSet) workloadTarget {
	return workloadTarget{
		Kind:       "StatefulSet",
		Name:       s.Name,
		Namespace:  s.Namespace,
		PolicyName: s.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers: s.Spec.Template.Spec.Containers,
		Selector:   s.Spec.Selector,
		Object:     s,
	}
}

func daemonSetToTarget(ds *appsv1.DaemonSet) workloadTarget {
	return workloadTarget{
		Kind:       "DaemonSet",
		Name:       ds.Name,
		Namespace:  ds.Namespace,
		PolicyName: ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers: ds.Spec.Template.Spec.Containers,
		Selector:   ds.Spec.Selector,
		Object:     ds,
	}
}

func rolloutToTarget(r *rolloutsv1alpha1.Rollout) workloadTarget {
	return workloadTarget{
		Kind:       "Rollout",
		Name:       r.Name,
		Namespace:  r.Namespace,
		PolicyName: r.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers: r.Spec.Template.Spec.Containers,
		Selector:   r.Spec.Selector,
		Object:     r,
	}
}

// filterTargets returns targets that match the given policy name and are not
// in the excluded namespaces list.
func filterTargets(targets []workloadTarget, policyName string, excludedNamespaces []string) []workloadTarget {
	var filtered []workloadTarget
	for _, t := range targets {
		if t.PolicyName != policyName {
			continue
		}
		if slices.Contains(excludedNamespaces, t.Namespace) {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}
