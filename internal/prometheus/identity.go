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

// attribute names the identity and container a shard query's series belongs
// to. A shard spans many identities, so a series missing any identity label,
// or the container label, is not attributed (false) rather than attributed to
// a neighbouring identity, which would silently corrupt that workload's
// recommendation.
func attribute(m model.Metric) (WorkloadIdentity, string, bool) {
	id := WorkloadIdentity{
		Namespace: string(m["namespace"]),
		OwnerKind: string(m["owner_kind"]),
		OwnerName: string(m["owner_name"]),
	}
	container := string(m["container"])
	if id.Namespace == "" || id.OwnerKind == "" || id.OwnerName == "" || container == "" {
		return WorkloadIdentity{}, "", false
	}
	return id, container, true
}

// shardSamples unpacks a shard query's vector, dropping every series it
// cannot attribute.
func shardSamples(vec model.Vector) []ShardSample {
	out := make([]ShardSample, 0, len(vec))
	for _, s := range vec {
		id, container, ok := attribute(s.Metric)
		if !ok {
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

// shardSeries unpacks a shard range query's matrix, dropping every series it
// cannot attribute.
func shardSeries(matrix model.Matrix) []ShardSeries {
	out := make([]ShardSeries, 0, len(matrix))
	for _, s := range matrix {
		id, container, ok := attribute(s.Metric)
		if !ok {
			continue
		}
		out = append(out, ShardSeries{
			Identity:  id,
			Container: container,
			Metric:    string(s.Metric[model.MetricNameLabel]),
			Points:    timeValues(s.Values),
		})
	}
	return out
}
