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
