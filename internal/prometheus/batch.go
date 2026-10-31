package prometheus

import (
	"context"
	"fmt"
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

// ShardSeries is one series of a shard range query's result: a ShardSample's
// series at every instant of the range it has a value at.
type ShardSeries struct {
	Identity  WorkloadIdentity
	Container string
	// Metric is the series' __name__, empty when a function in the query
	// dropped it.
	Metric string
	Points []TimeValue
}

// QueryShard runs one shard query as of at and returns its samples. It goes
// through execInstant rather than c.api.Query, so shard queries inherit the
// breaker, the in-flight semaphore and the query timeout.
func (c *Client) QueryShard(ctx context.Context, expr string, at time.Time) ([]ShardSample, error) {
	result, err := c.execInstant(ctx, expr, at, c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("prometheus shard query", expr, err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for shard query", result)
	}
	return shardSamples(vector), nil
}

// QueryShardRange runs one shard query at every instant of r stepping by
// step, each evaluated as QueryShard would as of it, and returns its series,
// attributed as QueryShard attributes its samples. It shares QueryShard's
// breaker, in-flight semaphore and query timeout.
func (c *Client) QueryShardRange(ctx context.Context, expr string, r TimeRange, step time.Duration) ([]ShardSeries, error) {
	matrix, err := c.execRange(ctx, expr, r, step, c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("prometheus shard range query", expr, err)
	}
	return shardSeries(matrix), nil
}
