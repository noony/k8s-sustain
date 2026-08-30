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

// Namespace annotations are fetched once, before the per-kind loop, and
// threaded into every listWorkloadsOfKind call — see listPolicyWorkloadRows
// for why: this loops over supportedWorkloadKinds too, and each call would
// otherwise re-List every Namespace in the cluster.
//
// Like listPolicyWorkloadRows, entryMatchesPolicy is the sole gate — opting
// in is necessary but not sufficient, see its doc. This function's only
// caller is handlePolicyBatchSimulate (batch_simulate.go), NOT the summary page's
// per-policy rollups — those come from Prometheus via fetchPolicyRollups
// (handlers_policies.go), reading controller-emitted metrics, which is why
// they were never affected by the dashboard-gating review finding in the
// first place. The gate here is still correct and worth keeping for batch
// simulate: without it, a Namespace naming this Policy would have every one
// of its workloads included in the simulation even when the Policy's own
// selector (or --excluded-namespaces) does not reach it.
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
