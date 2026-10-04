package prometheus

import (
	"cmp"
	"strings"

	"github.com/prometheus/common/model"
)

// WorkloadIdentity is the (namespace, owner_kind, owner_name) tuple that
// identifies a workload in every k8s_sustain recording rule. It keys batched
// query results, where one response carries many workloads and the container
// label alone is not unique.
type WorkloadIdentity struct {
	Namespace string
	OwnerKind string
	OwnerName string
}

// CompareIdentity orders identities by namespace, kind, then name, for callers
// that need a reproducible iteration order over a map keyed by identity.
func CompareIdentity(a, b WorkloadIdentity) int {
	return cmp.Or(
		strings.Compare(a.Namespace, b.Namespace),
		strings.Compare(a.OwnerKind, b.OwnerKind),
		strings.Compare(a.OwnerName, b.OwnerName),
	)
}

// shardSamples unpacks a shard query's vector. A shard spans many identities,
// so a series missing any identity label, or the container label, is dropped
// rather than attributed to a neighbouring identity, which would silently
// corrupt that workload's recommendation.
func shardSamples(vec model.Vector) []ShardSample {
	out := make([]ShardSample, 0, len(vec))
	for _, s := range vec {
		id := WorkloadIdentity{
			Namespace: string(s.Metric["namespace"]),
			OwnerKind: string(s.Metric["owner_kind"]),
			OwnerName: string(s.Metric["owner_name"]),
		}
		container := string(s.Metric["container"])
		if id.Namespace == "" || id.OwnerKind == "" || id.OwnerName == "" || container == "" {
			continue
		}
		out = append(out, ShardSample{
			Identity:  id,
			Container: container,
			Metric:    string(s.Metric[model.MetricNameLabel]),
			Value:     float64(s.Value),
		})
	}
	return out
}
