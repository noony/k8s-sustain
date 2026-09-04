// This is an external test package, unlike the rest of this directory, so it
// can import internal/config (the outermost CLI layer) without pointing the
// dependency arrow backwards from this low-level package.
package prometheus_test

import (
	"strconv"
	"testing"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/prometheus"
)

// Batching's whole justification is that 10k workloads cost tens of queries
// per cycle, not ten thousand. The other tests only exercise grouping and
// budget mechanics at toy scale, where a regression to near-per-workload
// sharding would still pass.
//
// The budget comes from config.DefaultQueryShardMaxSamples rather than a
// hand-copied literal so the two cannot drift. Thresholds are deliberately
// slack: they catch an order-of-magnitude regression, not a tuning change.
func TestShardScaleAtShippedDefaults(t *testing.T) {
	const (
		workloads  = 10_000
		containers = 3
	)
	budget := config.DefaultQueryShardMaxSamples

	shardCount := func(windowMinutes int) int {
		cands := make([]prometheus.ShardCandidate, 0, workloads)
		for i := range workloads {
			cands = append(cands, prometheus.ShardCandidate{
				Identity: prometheus.WorkloadIdentity{
					Namespace: "prod",
					OwnerKind: "Deployment",
					OwnerName: "workload-" + strconv.Itoa(i),
				},
				Containers: containers,
			})
		}
		shards, _ := prometheus.BuildShards(cands, windowMinutes, budget)
		return len(shards)
	}

	sevenDay := shardCount(prometheus.WindowMinutes("7d"))
	thirtyDay := shardCount(prometheus.WindowMinutes("30d"))
	t.Logf("10k workloads x %d containers: 7d window -> %d shards, 30d window -> %d shards",
		containers, sevenDay, thirtyDay)

	if sevenDay > 60 {
		t.Errorf("7d window produced %d shards for %d workloads; batching is no longer collapsing query count",
			sevenDay, workloads)
	}
	if thirtyDay > 200 {
		t.Errorf("30d window produced %d shards for %d workloads", thirtyDay, workloads)
	}

	// A longer window makes each workload cost proportionally more samples, so
	// fewer fit per shard. If this inverts, the cost formula has stopped
	// accounting for window length — which is exactly the bug that would let a
	// 30d query blow past Prometheus's --query.max-samples and be rejected
	// outright, failing every workload in the shard.
	if thirtyDay <= sevenDay {
		t.Errorf("30d needed %d shards but 7d needed %d; shard cost is not scaling with window length",
			thirtyDay, sevenDay)
	}
}
