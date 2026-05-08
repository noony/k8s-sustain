package controller

import (
	"slices"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// workloadTarget is the unit of work for reconciliation. It represents a single
// workload (Deployment, StatefulSet, DaemonSet, Rollout, CronJob) that matches
// a Policy.
type workloadTarget struct {
	Kind           string
	Name           string
	Namespace      string
	PolicyName     string
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Selector       *metav1.LabelSelector
	Object         client.Object
}

// key returns a unique identifier for this workload target, used as the retry map key.
func (w *workloadTarget) key() string { //nolint:unused // used in Task 5 reconcile rewrite
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

// recommendableContainers returns the containers to feed into the recommendation
// pipeline plus a set of container names that originate from InitContainers.
// When excludeInit is true (or the workload has no init containers), the init
// list is dropped and the returned set is empty.
func (w *workloadTarget) recommendableContainers(excludeInit bool) ([]corev1.Container, map[string]struct{}) {
	if excludeInit || len(w.InitContainers) == 0 {
		return w.Containers, nil
	}
	merged := make([]corev1.Container, 0, len(w.Containers)+len(w.InitContainers))
	merged = append(merged, w.Containers...)
	merged = append(merged, w.InitContainers...)
	initNames := make(map[string]struct{}, len(w.InitContainers))
	for _, c := range w.InitContainers {
		initNames[c.Name] = struct{}{}
	}
	return merged, initNames
}

func deploymentToTarget(d *appsv1.Deployment) workloadTarget {
	return workloadTarget{
		Kind:           "Deployment",
		Name:           d.Name,
		Namespace:      d.Namespace,
		PolicyName:     d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers:     d.Spec.Template.Spec.Containers,
		InitContainers: d.Spec.Template.Spec.InitContainers,
		Selector:       d.Spec.Selector,
		Object:         d,
	}
}

func statefulSetToTarget(s *appsv1.StatefulSet) workloadTarget {
	return workloadTarget{
		Kind:           "StatefulSet",
		Name:           s.Name,
		Namespace:      s.Namespace,
		PolicyName:     s.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers:     s.Spec.Template.Spec.Containers,
		InitContainers: s.Spec.Template.Spec.InitContainers,
		Selector:       s.Spec.Selector,
		Object:         s,
	}
}

func daemonSetToTarget(ds *appsv1.DaemonSet) workloadTarget {
	return workloadTarget{
		Kind:           "DaemonSet",
		Name:           ds.Name,
		Namespace:      ds.Namespace,
		PolicyName:     ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers:     ds.Spec.Template.Spec.Containers,
		InitContainers: ds.Spec.Template.Spec.InitContainers,
		Selector:       ds.Spec.Selector,
		Object:         ds,
	}
}

func rolloutToTarget(r *rolloutsv1alpha1.Rollout) workloadTarget {
	return workloadTarget{
		Kind:           "Rollout",
		Name:           r.Name,
		Namespace:      r.Namespace,
		PolicyName:     r.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers:     r.Spec.Template.Spec.Containers,
		InitContainers: r.Spec.Template.Spec.InitContainers,
		Selector:       r.Spec.Selector,
		Object:         r,
	}
}

// cronJobToTarget builds a workloadTarget from a CronJob. The opt-in policy
// annotation lives on the JobTemplate's pod template, matching the convention
// used for other kinds. Selector is left nil — CronJob reconciliation patches
// the JobTemplate directly and never lists/recycles pods.
func cronJobToTarget(c *batchv1.CronJob) workloadTarget {
	return workloadTarget{
		Kind:           "CronJob",
		Name:           c.Name,
		Namespace:      c.Namespace,
		PolicyName:     c.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation],
		Containers:     c.Spec.JobTemplate.Spec.Template.Spec.Containers,
		InitContainers: c.Spec.JobTemplate.Spec.Template.Spec.InitContainers,
		Selector:       nil,
		Object:         c,
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
