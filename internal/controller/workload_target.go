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
	"github.com/noony/k8s-sustain/internal/workload"
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
	return workload.MergeContainersForRecommendation(w.Containers, w.InitContainers, excludeInit)
}

// newTargetFromTemplate assembles a workloadTarget from a typed workload
// object and its pod template. Callers pass the template explicitly so this
// helper can stay agnostic of the rollouts API (workload.PodTemplateOf does
// not handle Argo Rollouts).
func newTargetFromTemplate(obj client.Object, kind string, tmpl *corev1.PodTemplateSpec, selector *metav1.LabelSelector) workloadTarget {
	t := workloadTarget{
		Kind:      kind,
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
		Selector:  selector,
		Object:    obj,
	}
	if tmpl != nil {
		t.PolicyName = tmpl.Annotations[sustainv1alpha1.PolicyAnnotation]
		t.Containers = tmpl.Spec.Containers
		t.InitContainers = tmpl.Spec.InitContainers
	}
	return t
}

func deploymentToTarget(d *appsv1.Deployment) workloadTarget {
	return newTargetFromTemplate(d, "Deployment", &d.Spec.Template, d.Spec.Selector)
}

func statefulSetToTarget(s *appsv1.StatefulSet) workloadTarget {
	return newTargetFromTemplate(s, "StatefulSet", &s.Spec.Template, s.Spec.Selector)
}

func daemonSetToTarget(ds *appsv1.DaemonSet) workloadTarget {
	return newTargetFromTemplate(ds, "DaemonSet", &ds.Spec.Template, ds.Spec.Selector)
}

func rolloutToTarget(r *rolloutsv1alpha1.Rollout) workloadTarget {
	return newTargetFromTemplate(r, "Rollout", &r.Spec.Template, r.Spec.Selector)
}

// cronJobToTarget builds a workloadTarget from a CronJob. The opt-in policy
// annotation lives on the JobTemplate's pod template, matching the convention
// used for other kinds. Selector is left nil — CronJob reconciliation patches
// the JobTemplate directly and never lists/recycles pods.
func cronJobToTarget(c *batchv1.CronJob) workloadTarget {
	return newTargetFromTemplate(c, "CronJob", &c.Spec.JobTemplate.Spec.Template, nil)
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
