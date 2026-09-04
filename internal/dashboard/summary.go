package dashboard

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

type automatedWorkload struct {
	Namespace      string
	Kind           string
	Name           string
	PolicyName     string
	Containers     []corev1.Container
	InitContainers []corev1.Container
}

// collectPolicyWorkloads returns every live workload the policy manages, for
// the batch simulation.
func (s *Server) collectPolicyWorkloads(ctx context.Context, policyName string, policy *sustainv1alpha1.Policy) []automatedWorkload {
	var workloads []automatedWorkload
	s.forEachPolicyEntry(ctx, policy, policyName, func(kind string, e workloadEntry) {
		workloads = append(workloads, automatedWorkload{
			Namespace:      e.Namespace,
			Kind:           kind,
			Name:           e.Name,
			PolicyName:     policyName,
			Containers:     e.Containers(),
			InitContainers: e.InitContainers(),
		})
	})
	return workloads
}
