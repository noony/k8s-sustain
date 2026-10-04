package prometheus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// ShardSample is one series of a shard query's result.
type ShardSample struct {
	Identity  WorkloadIdentity
	Container string
	// Metric is the series' __name__, empty when a function in the query
	// dropped it.
	Metric string
	Value  float64
}

// QueryShard runs one shard query at the current instant and returns its
// samples. It goes through execInstant rather than c.api.Query, so shard
// queries inherit the breaker, the in-flight semaphore and the query timeout.
func (c *Client) QueryShard(ctx context.Context, expr string) ([]ShardSample, error) {
	result, err := c.execInstant(ctx, expr, time.Now(), c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("prometheus shard query", expr, err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for shard query", result)
	}
	return shardSamples(vector), nil
}

// queryShardIdentityValues is the shared tail of QueryShardCPU and
// QueryShardMemory. It must go through execInstant rather than c.api.Query so
// shard queries inherit the breaker, in-flight semaphore and timeout.
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

// QueryShardCPU returns, for every workload in the shard, the per-container
// CPU quantile (cores) of the busiest replica over window, from the
// workload_max_pod_cpu rule, in one round trip.
func (c *Client) QueryShardCPU(ctx context.Context, shard Shard, quantile float64, window string) (IdentityValues, error) {
	expr := QuantileOverTime(quantile, MetricWorkloadMaxPodCPUCores, shard.Selector(), window)
	return c.queryShardIdentityValues(ctx, expr)
}

// QueryShardMemory is QueryShardCPU for memory (bytes), from the
// workload_max_pod_memory rule.
func (c *Client) QueryShardMemory(ctx context.Context, shard Shard, quantile float64, window string) (IdentityValues, error) {
	expr := QuantileOverTime(quantile, MetricWorkloadMaxPodMemoryBytes, shard.Selector(), window)
	return c.queryShardIdentityValues(ctx, expr)
}

// oomShardSelector builds the combined selector for a shard's OOM-signal query.
//
// oomMetricNames are joined unescaped because every rule name is RE2-literal
// (asserted by TestOOMSignalSelectorUsesLiteralAlternation). Owner names are
// not: they may contain '.', so they must go through
// literalAlternation(shard.Names) — joining shard.Names directly silently
// mismatches dotted names like "payments.worker".
func oomShardSelector(shard Shard) string {
	return fmt.Sprintf(`{__name__=~%q,namespace=%q,owner_kind=%q,owner_name=~%q}`,
		strings.Join(oomMetricNames, "|"), shard.Namespace, shard.OwnerKind, literalAlternation(shard.Names))
}

// partitionOOMVectorByIdentity splits a shard's combined OOM vector into one
// sub-vector per workload identity. Samples missing any identity label are
// dropped rather than folded into a neighbour, which would corrupt that
// neighbour's OOM signal.
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

// QueryShardOOMSignal returns, for every workload in the shard, the 24h
// per-container OOM counts, peak memory and OOM-time limit, fetched in one
// query and folded per identity by foldOOMVector.
//
// The returned map is sparse: it holds only identities with samples in the
// window, which for most shards is a small minority. Callers must treat a
// missing key as "no OOM signal" (the zero value), never as a failed lookup.
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
