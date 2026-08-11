// Package prometheus_test (an EXTERNAL test package, not "package
// prometheus") holds TestShardScaleAtShippedDefaults, which needs
// internal/config.DefaultQueryShardMaxSamples -- see the doc comment below
// for why that import has to live here rather than in the internal test
// package alongside the rest of this directory's tests.
package prometheus_test

import (
	"strconv"
	"testing"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/prometheus"
)

// The entire justification for batching is a load-reduction claim, and that
// claim is arithmetic on BuildShards: at 10k workloads the controller should
// issue tens of queries per resource per cycle, not ten thousand. Nothing else
// in the suite pins that — the unit tests above check grouping and budget
// mechanics at toy scale, where a regression to near-per-workload sharding
// would still pass.
//
// So this asserts the property at realistic scale, using
// config.DefaultQueryShardMaxSamples -- the actual shipped default for
// --query-shard-max-samples (internal/config/config.go) -- instead of a
// hand-copied literal, so the two can never silently drift apart the way a
// duplicated constant could.
//
// # Why this file is "package prometheus_test", not "package prometheus"
//
// Every other test in this directory is an in-package test ("package
// prometheus") so it can reach unexported identifiers. This file only needs
// BuildShards/ShardCandidate/WorkloadIdentity/WindowMinutes, which are all
// already exported, so it costs nothing to make it an external test package
// instead -- and that choice matters here: internal/prometheus is a
// low-level package that internal/recommender, internal/controller, the
// webhook, and the dashboard all build on, while internal/config is the
// outermost CLI/Viper wiring layer. Importing internal/config from
// "package prometheus" itself would point that dependency arrow backwards
// (and could not be undone by build tags -- production code and its
// in-package tests share one compiled unit for cycle purposes). An external
// test package is a separate compilation unit that nothing else imports, so
// it can safely depend on internal/config for this one cross-check without
// internal/prometheus's production code -- or its in-package tests --
// gaining that dependency. (There is also no literal import cycle here:
// internal/config only depends on api/v1alpha1, which does not depend on
// internal/prometheus. But "not a cycle" and "not a layering inversion" are
// different questions, and this design avoids both rather than relying on
// only the first one holding today.)
//
// If someone lowers that budget, changes the cost formula, or makes the budget
// check off-by-one in a way that closes shards early, this fails loudly instead
// of quietly restoring the fan-out the whole plan exists to remove.
//
// Thresholds are deliberately slack — they catch an order-of-magnitude
// regression, not a small tuning change.
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
