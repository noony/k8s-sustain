package governance

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Policies is every Policy that may govern, with its label selector compiled
// once, and the namespaces none of them may govern.
type Policies struct {
	byName   map[string]*compiled
	excluded []string
}

type compiled struct {
	policy *sustainv1alpha1.Policy
	// selector is nil when the Policy has none, which matches every label set.
	selector labels.Selector
	invalid  bool
}

// NewPolicies indexes items, which it keeps: a governing Policy is returned as
// a pointer into items.
func NewPolicies(items []sustainv1alpha1.Policy, excludedNamespaces []string) *Policies {
	ps := &Policies{byName: make(map[string]*compiled, len(items)), excluded: excludedNamespaces}
	for i := range items {
		c := &compiled{policy: &items[i]}
		if ls := items[i].Spec.Selector.LabelSelector; ls != nil {
			sel, err := metav1.LabelSelectorAsSelector(ls)
			c.selector, c.invalid = sel, err != nil
		}
		ps.byName[items[i].Name] = c
	}
	return ps
}

// consent returns the Policy name when it exists and accepts namespace and,
// when matchLabels, podLabels; otherwise why not.
func (ps *Policies) consent(name, namespace string, podLabels map[string]string, matchLabels bool) (*sustainv1alpha1.Policy, Refusal) {
	c, ok := ps.byName[name]
	if !ok {
		return nil, PolicyMissing
	}
	if refusal := ps.refusal(c, namespace, podLabels, matchLabels); refusal != "" {
		return nil, refusal
	}
	return c.policy, ""
}

// anySelects is the pre-gate: whether some Policy accepts namespace and
// podLabels, by the same rule consent applies.
func (ps *Policies) anySelects(namespace string, podLabels map[string]string) Refusal {
	if slices.Contains(ps.excluded, namespace) {
		return NamespaceExcluded
	}
	for _, c := range ps.byName {
		if ps.refusal(c, namespace, podLabels, true) == "" {
			return ""
		}
	}
	return NoPolicySelects
}

func (ps *Policies) refusal(c *compiled, namespace string, podLabels map[string]string, matchLabels bool) Refusal {
	allowed := c.policy.Spec.Selector.Namespaces
	switch {
	case slices.Contains(ps.excluded, namespace):
		return NamespaceExcluded
	case len(allowed) > 0 && !slices.Contains(allowed, namespace):
		return NamespaceNotSelected
	case !matchLabels:
		return ""
	case c.invalid:
		return SelectorInvalid
	case c.selector != nil && !c.selector.Matches(labels.Set(podLabels)):
		return LabelsNotSelected
	}
	return ""
}
