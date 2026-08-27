package workload

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// BarePodGroup is one (namespace, owner-name) identity formed by grouping
// bare pods (no controller owner) that share a valid k8s.sustain.io/owner-name
// annotation. See ApplyOwnerNameOverride and GroupBarePods.
type BarePodGroup struct {
	Namespace      string
	Name           string // the owner-name annotation value
	PolicyName     string
	Labels         map[string]string
	Containers     []corev1.Container
	InitContainers []corev1.Container
	// Representative is the most recently created pod in the group, used by
	// callers that need its full ObjectMeta (e.g. annotations) or identity.
	Representative *corev1.Pod
	// Members is every pod in the group, in list order. Callers that act on
	// the group (in-place resize) need all of them, not just the
	// representative — and they must not re-derive membership themselves,
	// because the rule (no controller owner, valid owner-name, matching
	// policy annotation) is exactly the kind of logic that drifts when it has
	// two implementations. The pods are pointers into the slice passed to
	// GroupBarePods, like Representative.
	Members []*corev1.Pod
	// PolicyMismatched holds the pods that share this group's
	// (namespace, owner-name) but name a DIFFERENT policy in their
	// k8s.sustain.io/policy annotation. They are excluded from Members,
	// Representative and Containers — see GroupBarePods — and recorded here so
	// the caller can tell an operator, because a pod that opted in and is then
	// silently ignored is its own bug.
	PolicyMismatched []*corev1.Pod
}

// GroupBarePods groups pods with no controller owner and a valid owner-name
// annotation by (namespace, owner-name), per the spec's namespace-scoping
// rule. Pods with a real controller owner, or without a valid owner-name
// annotation, are excluded — they're either handled by another kind's
// listing, or are anonymous bare pods that opt into nothing.
//
// The most recently created pod in each group supplies the representative
// containers: an older pod's spec may be stale (e.g. after a DAG code
// change), and the newest admitted pod is the freshest signal of what the
// workload's containers currently look like. Shared by the controller's
// listBarePodTargets and the dashboard's listWorkloadsOfKind/getWorkloadEntry
// so the grouping rule has exactly one implementation.
//
// # One identity, one policy
//
// A group is claimed by the policy of the FIRST qualifying pod seen; pods
// sharing its (namespace, owner-name) that name a different policy are put in
// PolicyMismatched and excluded from Members, Representative and Containers.
//
// They cannot simply form their own group: the identity IS (namespace,
// owner-name), so both would map to the same wlrcache.Name and fight over one
// WorkloadRecommendation. Two policies claiming one owner-name is a genuine
// ambiguity in the cluster's configuration, and the only safe resolution is to
// let one win and report the other rather than to average them.
//
// Excluding rather than absorbing matters because the group is now WRITTEN to,
// not just read: the controller feeds Members straight to ResizePodsInPlace
// (see internal/controller/barepod_resize.go), so an absorbed pod would be
// resized in place under another policy's recommendation — and for memory an
// in-place resize can restart the container. Every other kind checks ownerRef
// UID and selector before the patcher touches a pod; this is that check.
func GroupBarePods(pods []corev1.Pod) []BarePodGroup {
	groups := map[string]*BarePodGroup{} // key: namespace + "/" + owner-name
	var order []string
	for i := range pods {
		p := &pods[i]
		if metav1.GetControllerOf(p) != nil {
			continue // has a real controller owner — handled elsewhere
		}
		policyName := p.Annotations[sustainv1alpha1.PolicyAnnotation]
		if policyName == "" {
			continue
		}
		// Passing kind="" lets ApplyOwnerNameOverride double as a validity
		// check here: it returns kind="" unchanged when the annotation is
		// absent or fails label-value validation, and "Pod" when it's valid.
		effKind, effName := ApplyOwnerNameOverride("", "", p.Annotations)
		if effKind == "" {
			continue // no valid owner-name annotation — anonymous bare pod
		}
		key := p.Namespace + "/" + effName
		g, ok := groups[key]
		if !ok {
			g = &BarePodGroup{
				Namespace: p.Namespace,
				Name:      effName,
				// Taken from the first pod encountered in the group. Pods
				// within a group are expected to reference the same Policy;
				// the ones that do not are excluded below rather than
				// absorbed, because this group is applied to, not just read.
				PolicyName: policyName,
				Labels:     p.Labels,
			}
			groups[key] = g
			order = append(order, key)
		}
		if policyName != g.PolicyName {
			g.PolicyMismatched = append(g.PolicyMismatched, p)
			continue
		}
		if g.Representative == nil || p.CreationTimestamp.After(g.Representative.CreationTimestamp.Time) {
			g.Representative = p
			g.Containers = p.Spec.Containers
			g.InitContainers = p.Spec.InitContainers
		}
		g.Members = append(g.Members, p)
	}
	out := make([]BarePodGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out
}
