package workload

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/noony/k8s-sustain/internal/policymatch"
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
	// Members is every pod in the group, in list order, pointing into the slice
	// passed to GroupBarePods. Callers that act on the group must use this
	// rather than re-deriving membership, so the rule has one implementation.
	Members []*corev1.Pod
	// PolicyMismatched holds pods sharing this group's (namespace, owner-name)
	// that name a different policy. They are excluded from Members,
	// Representative and Containers, and recorded here so the caller can report
	// them — a pod that opted in and is then silently ignored is its own bug.
	PolicyMismatched []*corev1.Pod
}

// GroupBarePods groups pods with no controller owner and a valid owner-name
// annotation by (namespace, owner-name). A pod opts in through its own
// annotations or its Namespace's (nsAnnotations maps namespace name to those
// annotations; nil simply means no namespace-level opt-in). Shared by the
// controller and the dashboard so the grouping rule has one implementation.
//
// The most recently created pod supplies the representative containers: an
// older pod's spec may be stale, e.g. after a DAG code change.
//
// # One identity, one policy
//
// A group is claimed by the policy of the FIRST qualifying pod seen; pods
// naming a different policy go to PolicyMismatched. They cannot form their own
// group — the identity IS (namespace, owner-name), so both would fight over one
// WorkloadRecommendation. Excluding rather than absorbing them matters because
// the controller feeds Members straight to ResizePodsInPlace, so an absorbed
// pod would be resized under another policy's recommendation, which for memory
// can restart the container.
func GroupBarePods(pods []corev1.Pod, nsAnnotations map[string]map[string]string) []BarePodGroup {
	groups := map[string]*BarePodGroup{} // key: namespace + "/" + owner-name
	var order []string
	for i := range pods {
		p := &pods[i]
		if metav1.GetControllerOf(p) != nil {
			continue // has a real controller owner — handled elsewhere
		}
		// A bare Pod has no pod template, so its own annotations ARE the most
		// specific level; the workload level is nil because there is no
		// separate object to carry it.
		policyName, _ := policymatch.ResolvePolicy(p.Annotations, nil, nsAnnotations[p.Namespace])
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
				Namespace:  p.Namespace,
				Name:       effName,
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
