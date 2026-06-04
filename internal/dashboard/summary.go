package dashboard

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// ---- Internal types ----

type automatedWorkload struct {
	Namespace  string
	Kind       string
	Name       string
	PolicyName string
	Containers []corev1.Container
}

// ---- Workload collection helpers ----

func (s *Server) collectPolicyWorkloads(ctx context.Context, policyName string, policy *sustainv1alpha1.Policy) []automatedWorkload {
	var workloads []automatedWorkload

	for _, kind := range supportedWorkloadKinds {
		if !kindEnabledInPolicy(policy, kind) {
			continue
		}
		entries, err := s.listWorkloadsOfKind(ctx, kind)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.PolicyAnnotation() != policyName {
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
