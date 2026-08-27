package prometheus

import "github.com/prometheus/common/model"

// WorkloadIdentity is the (namespace, owner_kind, owner_name) tuple that
// identifies a workload in every k8s_sustain recording rule. It is the map key
// for batched query results, where a single response carries many workloads and
// the container label alone is no longer unique.
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
// This is the batched counterpart of vectorToContainerValues, which reads only
// the container label because its query was pinned to one workload by the
// selector. Here many workloads share one response, so container alone is not a
// unique key: prod/api/app and staging/api/app would overwrite each other.
//
// Series missing ANY identity label, or missing container, are dropped. They
// cannot be attributed to a requesting workload, and folding them into a
// neighbouring identity would silently corrupt that workload's recommendation —
// far worse than the workload simply having no data this cycle, which callers
// already handle.
//
// Unlike foldOOMVector, this does NOT aggregate multiple samples that land on
// the same (identity, container) key — it last-write-wins. That is safe only
// because the CPU/memory recording rules this feeds are already aggregated
// server-side by `by (namespace, owner_kind, owner_name, container)` (see
// avgByContainer/maxByContainer and the rule definitions in
// charts/k8s-sustain/values.yaml), so at most one series per key can ever be
// returned — the same invariant vectorToContainerValues has relied on all
// along. If a rule's `by()` clause ever drops one of those labels, this stops
// being true silently; treat that invariant as load-bearing before touching
// this function or those rules.
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
