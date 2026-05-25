// Package policymatch holds the predicate that decides whether a Policy
// applies to a given namespace + workload label set. It lives in its own
// package so the controller (when listing/filtering targets) and the webhook
// (at pod admission time) can stay in lockstep on selector semantics without
// either side depending on the other.
package policymatch

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Matches reports whether the policy applies to a workload (or pod) at the
// given namespace with the given labels. It honours, in order:
//
//   - excludedNamespaces (a hard deny — the policy never applies there)
//   - policy.Spec.Selector.Namespaces (empty/nil means "all namespaces")
//   - policy.Spec.Selector.LabelSelector (nil or empty means "match all")
//
// A malformed LabelSelector is treated as non-matching. Callers that want to
// fail open on parse errors should call SelectorOK first.
func Matches(policy *sustainv1alpha1.Policy, namespace string, podLabels map[string]string, excludedNamespaces []string) bool {
	if policy == nil {
		return false
	}
	if slices.Contains(excludedNamespaces, namespace) {
		return false
	}
	if !namespaceAllowed(policy.Spec.Selector.Namespaces, namespace) {
		return false
	}
	sel, err := SelectorOK(policy.Spec.Selector.LabelSelector)
	if err != nil {
		return false
	}
	if sel == nil {
		return true
	}
	return sel.Matches(labels.Set(podLabels))
}

// SelectorOK parses the policy's LabelSelector. Returns (nil, nil) when the
// selector is nil (treated as match-everything by the caller). The parse error
// is returned so callers that want to fail open (e.g. the admission webhook)
// can distinguish "no selector" from "broken selector" and log accordingly.
func SelectorOK(ls *metav1.LabelSelector) (labels.Selector, error) {
	if ls == nil {
		return nil, nil
	}
	return metav1.LabelSelectorAsSelector(ls)
}

func namespaceAllowed(allowed []string, namespace string) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.Contains(allowed, namespace)
}
