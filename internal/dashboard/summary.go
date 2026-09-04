package dashboard

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

type automatedWorkload struct {
	Namespace  string
	Kind       string
	Name       string
	PolicyName string
	Containers []corev1.Container
}

// Namespace annotations are fetched once, before the per-kind loop: each
// listWorkloadsOfKind call would otherwise re-List every Namespace.
//
// entryMatchesPolicy is the sole gate — opting in is necessary but not
// sufficient. Without it, a Namespace naming this Policy would pull all of its
// workloads into the simulation even when the Policy's own selector (or
// --excluded-namespaces) does not reach them.
func (s *Server) collectPolicyWorkloads(ctx context.Context, policyName string, policy *sustainv1alpha1.Policy) []automatedWorkload {
	var workloads []automatedWorkload

	nsAnnotations, err := s.namespaceAnnotations(ctx)
	if err != nil {
		s.Logger.Error(err, "failed to list namespaces; namespace-level policy opt-in will not be resolved", "policy", policyName)
		nsAnnotations = nil
	}

	for _, kind := range supportedWorkloadKinds {
		if !kindEnabledInPolicy(policy, kind) {
			continue
		}
		entries, err := s.listWorkloadsOfKind(ctx, kind, nsAnnotations)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !entryMatchesPolicy(policy, e, s.ExcludedNamespaces) {
				continue
			}
			workloads = append(workloads, automatedWorkload{
				Namespace:  e.Namespace,
				Kind:       kind,
				Name:       e.Name,
				PolicyName: policyName,
				Containers: e.Containers(),
			})
		}
	}

	return workloads
}
