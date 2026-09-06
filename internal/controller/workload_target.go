package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
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
	// Labels is the workload object's metadata.labels. Used by filterTargets
	// to evaluate Policy.Spec.Selector.LabelSelector.
	Labels map[string]string
	// TemplateAnnotations and ObjectAnnotations are the two workload-owned
	// annotation levels, carried rather than resolved at construction because
	// the third level (the Namespace) needs an apiserver read that a pure
	// conversion function has no business doing. collectTargets resolves all
	// three into PolicyName; see policymatch.ResolvePolicy.
	TemplateAnnotations map[string]string
	ObjectAnnotations   map[string]string
	Object              client.Object
	// IdentityKind and IdentityName are the kind/name used for Prometheus
	// queries and WorkloadRecommendation naming. Equal to Kind/Name unless
	// the pod template carries a valid k8s.sustain.io/owner-name annotation
	// (api/v1alpha1.OwnerNameAnnotation), in which case they reflect the
	// override. Kind/Name themselves always stay the real object identity —
	// used for key(), recycling (Selector/Object), and event/log attribution.
	IdentityKind string
	IdentityName string
	// UpdateMode is the policy's per-kind mode this target was collected
	// under. OnCreate targets get recommendations and WLR cache writes but
	// are never recycled or resized — the webhook applies resources at pod
	// admission.
	UpdateMode sustainv1alpha1.UpdateMode
	// BarePodMembers is every live pod of a bare-pod identity, set only for
	// Kind == "Pod" targets by listBarePodTargets.
	//
	// It is carried on the target rather than re-derived at apply time: the
	// listing already Lists the namespace's pods and runs workload.GroupBarePods
	// over them, so re-deriving cost one namespace-wide, cache-backed pod List
	// per identity per cycle, issued concurrently under the errgroup.
	//
	// Every other kind resolves its pods from a label selector at apply time;
	// bare pods cannot, because membership is the (no controller owner, valid
	// owner-name, matching policy) rule rather than a selector.
	BarePodMembers []*corev1.Pod
	// BarePodPolicyMismatched carries workload.BarePodGroup.PolicyMismatched
	// through to collectTargets, set only for Kind == "Pod" targets whose group
	// excluded at least one pod naming a different policy. It rides on the
	// target rather than being logged in listBarePodTargets, which runs once per
	// policy whose selector merely covers the namespace — before filterTargets
	// narrows the result to the group's own policy. See the log call in
	// collectTargets.
	BarePodPolicyMismatched []*corev1.Pod
}

// key returns a unique identifier for this workload target, used as the retry map key.
func (w *workloadTarget) key() string {
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

// recommendableContainers returns the containers to feed into the recommendation
// pipeline plus a set of container names that originate from InitContainers.
// When excludeInit is true (or the workload has no init containers), the init
// list is dropped and the returned set is empty.
func (w *workloadTarget) recommendableContainers(excludeInit bool) ([]corev1.Container, map[string]struct{}) {
	return workload.MergeContainersForRecommendation(w.Containers, w.InitContainers, excludeInit)
}

// targetFromObject assembles a workloadTarget from a typed workload object.
// The pod template and selector come from workload.PodTemplateOf, so every
// kind is converted by the same rule; an unsupported object yields a target
// with no containers, which the recommendation pipeline treats as empty.
func targetFromObject(obj client.Object, kind string) workloadTarget {
	t := workloadTarget{
		Kind:              kind,
		Name:              obj.GetName(),
		Namespace:         obj.GetNamespace(),
		Labels:            obj.GetLabels(),
		ObjectAnnotations: obj.GetAnnotations(),
		Object:            obj,
		IdentityKind:      kind,
		IdentityName:      obj.GetName(),
	}
	tmpl, selector, ok := workload.PodTemplateOf(obj)
	if !ok {
		return t
	}
	t.Selector = selector
	t.TemplateAnnotations = tmpl.Annotations
	t.Containers = tmpl.Spec.Containers
	t.InitContainers = tmpl.Spec.InitContainers
	// The identity override stays pod-template-only: it is mirrored onto a
	// pod LABEL by the webhook for kube-state-metrics, so it has to live
	// where pods inherit it. Opt-in has no such constraint.
	t.IdentityKind, t.IdentityName = workload.ApplyOwnerNameOverride(kind, obj.GetName(), tmpl.Annotations)
	return t
}

// filterTargets returns targets that match the given policy: the workload's
// pod template carries the policy annotation, the workload's namespace is
// allowed by Policy.Spec.Selector.Namespaces (and not in excludedNamespaces),
// and the workload's labels satisfy Policy.Spec.Selector.LabelSelector. The
// webhook applies the same predicate at pod admission time so the two
// components stay in lockstep on what a Policy targets.
func filterTargets(targets []workloadTarget, policy *sustainv1alpha1.Policy, excludedNamespaces []string) []workloadTarget {
	if policy == nil {
		return nil
	}
	var filtered []workloadTarget
	for _, t := range targets {
		if t.PolicyName != policy.Name {
			continue
		}
		if !policymatch.Matches(policy, t.Namespace, t.Labels, excludedNamespaces) {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}
