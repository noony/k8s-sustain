package prometheus

import "github.com/prometheus/common/model"

// WorkloadIdentity is the (namespace, owner_kind, owner_name) tuple that
// identifies a workload in every k8s_sustain recording rule. It keys batched
// query results, where one response carries many workloads and the container
// label alone is not unique.
type WorkloadIdentity struct {
	Namespace string
	OwnerKind string
	OwnerName string
}

// IdentityValues maps a workload identity to its per-container values.
type IdentityValues map[WorkloadIdentity]ContainerValues

// vectorToIdentityValues unpacks a batched instant-query vector, grouping by
// the full workload identity.
//
// Series missing any identity label are dropped rather than folded into a
// neighbouring identity, which would silently corrupt that workload's
// recommendation.
//
// Duplicate (identity, container) keys are last-write-wins. That is safe only
// because the recording rules this feeds already aggregate server-side by
// `by (namespace, owner_kind, owner_name, container)` (see
// charts/k8s-sustain/values.yaml), so at most one series per key can be
// returned. If a rule's `by()` clause ever drops one of those labels, this
// breaks silently.
func vectorToIdentityValues(vec model.Vector) IdentityValues {
	out := make(IdentityValues)
	for _, s := range vec {
		container := string(s.Metric["container"])
		if container == "" {
			continue
		}
		id := WorkloadIdentity{
			Namespace: string(s.Metric["namespace"]),
			OwnerKind: string(s.Metric["owner_kind"]),
			OwnerName: string(s.Metric["owner_name"]),
		}
		if id.Namespace == "" || id.OwnerKind == "" || id.OwnerName == "" {
			continue
		}
		cv, ok := out[id]
		if !ok {
			cv = ContainerValues{}
			out[id] = cv
		}
		cv[container] = float64(s.Value)
	}
	return out
}
