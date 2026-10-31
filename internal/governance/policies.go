package governance

import (
	"context"
	"fmt"
	"slices"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Policies is every Policy that may govern, with its label selector compiled,
// and the namespaces none of them may govern. Nothing modifies it once built,
// so concurrent decisions can share it.
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

// PolicyLister lists every Policy for a reader, compiling a Policy's label
// selector once per stored version of it, so a reader that lists on every
// admission or event recompiles only what changed. The zero value is ready
// and safe for concurrent use.
type PolicyLister struct {
	mu   sync.Mutex
	last map[string]*compiled
}

// List lists every Policy through r. It lists without deep copies, since the
// webhook lists on every admission: from an informer cache, a Policy it
// returns shares its fields with the cache's own object, and with every other
// call that saw the same version of it. Never modify it.
func (l *PolicyLister) List(ctx context.Context, r client.Reader, excludedNamespaces []string) (*Policies, error) {
	var list sustainv1alpha1.PolicyList
	if err := r.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing policies: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ps := index(list.Items, excludedNamespaces, l.last)
	l.last = ps.byName
	return ps, nil
}

// index reuses what prev compiled for a Policy whose UID and resourceVersion
// are unchanged. A Policy without a resourceVersion was never stored, so
// nothing vouches that its content is the one compiled.
func index(items []sustainv1alpha1.Policy, excluded []string, prev map[string]*compiled) *Policies {
	ps := &Policies{byName: make(map[string]*compiled, len(items)), excluded: excluded}
	for i := range items {
		p := &items[i]
		if c, ok := prev[p.Name]; ok && p.ResourceVersion != "" &&
			c.policy.UID == p.UID && c.policy.ResourceVersion == p.ResourceVersion {
			ps.byName[p.Name] = c
			continue
		}
		c := &compiled{policy: p}
		if ls := p.Spec.Selector.LabelSelector; ls != nil {
			sel, err := metav1.LabelSelectorAsSelector(ls)
			c.selector, c.invalid = sel, err != nil
		}
		ps.byName[p.Name] = c
	}
	return ps
}

// consent returns the Policy called name when it exists and accepts namespace
// and podLabels; otherwise why not.
func (ps *Policies) consent(name, namespace string, podLabels map[string]string) (*sustainv1alpha1.Policy, Refusal) {
	c, refusal := ps.namespaceConsent(name, namespace)
	if refusal != "" {
		return nil, refusal
	}
	if refusal := c.labelRefusal(podLabels); refusal != "" {
		return nil, refusal
	}
	return c.policy, ""
}

// departedConsent is consent for a Departed identity, which has no labels to
// match: a selector that does not parse still refuses it, as it refuses every
// live member.
func (ps *Policies) departedConsent(name, namespace string) (*sustainv1alpha1.Policy, Refusal) {
	c, refusal := ps.namespaceConsent(name, namespace)
	switch {
	case refusal != "":
		return nil, refusal
	case c.invalid:
		return nil, SelectorInvalid
	}
	return c.policy, ""
}

// namespaceConsent returns the Policy called name when it exists and accepts
// namespace; otherwise why not.
func (ps *Policies) namespaceConsent(name, namespace string) (*compiled, Refusal) {
	c, ok := ps.byName[name]
	if !ok {
		return nil, PolicyMissing
	}
	if refusal := ps.namespaceRefusal(c, namespace); refusal != "" {
		return nil, refusal
	}
	return c, ""
}

// gateRefusal is the pre-gate: the refusal every Policy gives namespace and
// podLabels by the rule consent applies, empty when some Policy may govern
// them, as one whose selector does not parse may.
func (ps *Policies) gateRefusal(namespace string, podLabels map[string]string) Refusal {
	if slices.Contains(ps.excluded, namespace) {
		return NamespaceExcluded
	}
	for _, c := range ps.byName {
		if ps.namespaceRefusal(c, namespace) == "" && c.labelRefusal(podLabels) != LabelsNotSelected {
			return ""
		}
	}
	return NoPolicySelects
}

func (ps *Policies) namespaceRefusal(c *compiled, namespace string) Refusal {
	allowed := c.policy.Spec.Selector.Namespaces
	switch {
	case slices.Contains(ps.excluded, namespace):
		return NamespaceExcluded
	case len(allowed) > 0 && !slices.Contains(allowed, namespace):
		return NamespaceNotSelected
	}
	return ""
}

func (c *compiled) labelRefusal(podLabels map[string]string) Refusal {
	switch {
	case c.invalid:
		return SelectorInvalid
	case c.selector != nil && !c.selector.Matches(labels.Set(podLabels)):
		return LabelsNotSelected
	}
	return ""
}
