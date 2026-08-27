package prometheus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// queryShardIdentityValues runs expr through execInstant and decodes it with
// vectorToIdentityValues — the shared tail of QueryShardCPU and
// QueryShardMemory, mirroring queryByContainer's role for the single-workload
// queries. Going through execInstant (rather than calling c.api.Query
// directly) is what makes these methods inherit the breaker, in-flight
// semaphore and timeout behaviour every other query path gets; do not bypass
// it here.
func (c *Client) queryShardIdentityValues(ctx context.Context, expr string) (IdentityValues, error) {
	result, err := c.execInstant(ctx, expr, time.Now(), c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("prometheus shard query", expr, err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for shard query", result)
	}
	return vectorToIdentityValues(vector), nil
}

// QueryShardCPU is the batched counterpart of QueryWorkloadCPUByContainer: the
// same per-pod-percentile semantics (see that method's doc comment), but
// scoped to every workload in shard with one round trip instead of one query
// per workload. Reads the k8s_sustain:workload_max_pod_cpu:cores recording
// rule and decodes the response by full workload identity, since a shard's
// response carries many workloads and container alone can no longer
// disambiguate them (see vectorToIdentityValues).
func (c *Client) QueryShardCPU(ctx context.Context, shard Shard, quantile float64, window string) (IdentityValues, error) {
	expr := quantileOverTimeExpr(quantile, MetricWorkloadMaxPodCPUCores, shard.Selector(), window)
	return c.queryShardIdentityValues(ctx, expr)
}

// QueryShardMemory is the batched counterpart of QueryWorkloadMemoryByContainer.
// Reads the k8s_sustain:workload_max_pod_memory:bytes recording rule. Same
// per-pod-percentile, per-identity-decoding semantics as QueryShardCPU.
func (c *Client) QueryShardMemory(ctx context.Context, shard Shard, quantile float64, window string) (IdentityValues, error) {
	expr := quantileOverTimeExpr(quantile, MetricWorkloadMaxPodMemoryBytes, shard.Selector(), window)
	return c.queryShardIdentityValues(ctx, expr)
}

// oomShardSelector builds the combined selector
// `{__name__=~"a|b|c",namespace=…,owner_kind=…,owner_name=~<alternation>}`
// for a shard's OOM-signal query.
//
// The __name__ alternation reuses oomMetricNames unescaped, exactly as
// oomSignalSelector does for the single-workload query — safe for the same
// reason documented there: every rule name is composed of RE2-literal
// characters. The guard for that is the call-site-agnostic property assertion
// in TestOOMSignalSelectorUsesLiteralAlternation, which checks the names
// themselves rather than any one selector's rendered output — so it protects
// this call site too, even though it never invokes it.
//
// The owner_name alternation, by contrast, MUST go through
// shard.escapedNameAlternation() rather than joining shard.Names directly.
// This is the exact trap that produced a real review-caught bug: a
// standalone OOM selector built independently of Shard.Selector() has no
// reason to remember that owner names need regexp.QuoteMeta, and silently
// drops or mismatches any dotted name (e.g. "payments.worker"). Routing both
// selectors through the same escapedNameAlternation method is what makes
// that drift impossible instead of merely unlikely.
func oomShardSelector(shard Shard) string {
	return fmt.Sprintf(`{__name__=~%q,namespace=%q,owner_kind=%q,owner_name=~%q}`,
		strings.Join(oomMetricNames, "|"), shard.Namespace, shard.OwnerKind, shard.escapedNameAlternation())
}

// partitionOOMVectorByIdentity splits a shard's combined OOM vector into one
// sub-vector per workload identity, so each can be folded independently by
// foldOOMVector. Samples missing any identity label are dropped — same
// reasoning as vectorToIdentityValues: they cannot be attributed to a
// requesting workload, and folding them into a neighbour would silently
// corrupt that neighbour's OOM signal.
func partitionOOMVectorByIdentity(vec model.Vector) map[WorkloadIdentity]model.Vector {
	out := make(map[WorkloadIdentity]model.Vector)
	for _, s := range vec {
		id := WorkloadIdentity{
			Namespace: string(s.Metric["namespace"]),
			OwnerKind: string(s.Metric["owner_kind"]),
			OwnerName: string(s.Metric["owner_name"]),
		}
		if id.Namespace == "" || id.OwnerKind == "" || id.OwnerName == "" {
			continue
		}
		out[id] = append(out[id], s)
	}
	return out
}

// QueryShardOOMSignal is the batched counterpart of QueryWorkloadOOMSignal:
// one query fetches all three OOM recording-rule families for every workload
// in shard, instead of one query per workload.
//
// The response is partitioned by workload identity (partitionOOMVectorByIdentity)
// and each partition is folded independently with the existing foldOOMVector —
// the same function the single-workload path uses. Reusing it here, rather
// than reimplementing the sum-for-counts/max-for-peak aggregation, is
// required: it is what guarantees the single-workload and batched paths
// produce byte-identical results for the same underlying series, so the two
// can never quietly drift apart as the aggregation rules evolve.
//
// THE RETURNED MAP IS SPARSE. It contains an entry only for identities that
// actually have OOM, peak-memory or OOM-limit samples in the window — a shard
// of 200 workloads where 2 have ever OOMed returns 2 entries, not 200. This is
// the normal, overwhelmingly common case, not an error: most workloads never
// OOM. Callers MUST treat a missing key as "no OOM signal" and fall back to
// the zero OOMSignal, never as a failed lookup. Reading a missing key from a
// Go map yields the zero value, so the natural `sigs[id]` already does the
// right thing — but do not write code that errors, warns, or retries on
// absence.
func (c *Client) QueryShardOOMSignal(ctx context.Context, shard Shard) (map[WorkloadIdentity]OOMSignal, error) {
	expr := oomShardSelector(shard)
	result, err := c.execInstant(ctx, expr, time.Now(), c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("shard oom signal query", expr, err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for shard oom signal", result)
	}

	partitions := partitionOOMVectorByIdentity(vector)
	out := make(map[WorkloadIdentity]OOMSignal, len(partitions))
	for id, v := range partitions {
		out[id] = foldOOMVector(v)
	}
	return out, nil
}
