package governance

import sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"

// NewPolicies indexes items for a test Reader, which serves Policies without
// listing them through a client.
func NewPolicies(items []sustainv1alpha1.Policy, excludedNamespaces []string) *Policies {
	return index(items, excludedNamespaces, nil)
}
