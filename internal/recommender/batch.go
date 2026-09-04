package recommender

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// shardFetchConcurrency bounds concurrent shard queries within one pass. It is
// a goroutine bound only; the client's in-flight semaphore throttles Prometheus.
const shardFetchConcurrency = 8

// fallbackFetchConcurrency bounds per-workload fallback queries for one shard.
// A shard can hold ~1000 workloads and the over-budget failure that triggers
// the fallback is deterministic, so a serial walk could outlast the reconcile
// interval. Kept below shardFetchConcurrency because the two multiply.
const fallbackFetchConcurrency = 4

// BatchInputs maps each requested workload identity to its fetched inputs.
// Every requested identity has a non-nil entry; a missing key means it was
// never requested.
type BatchInputs map[promclient.WorkloadIdentity]*WorkloadInputs

// BatchStats carries per-identity fetch outcomes that BatchInputs cannot
// express, since an empty entry looks the same whether the fetch failed or
// simply found no samples.
type BatchStats struct {
	// Failures holds the error for identities whose shard query and
	// per-workload fallback both failed. Errors are stored unwrapped, so
	// promclient.ErrCircuitOpen stays detectable with errors.Is. OOM failures
	// are never recorded; OOM is best-effort.
	Failures map[promclient.WorkloadIdentity]error
}

// FetchWorkloadInputsBatch is the sharded counterpart of FetchWorkloadInputs:
// it groups cands into batched shard queries and fans the results back out to
// one WorkloadInputs per requested identity. It never returns an error: a
// failed shard is retried once, then falls back to per-workload fetches, and
// a failed fallback is recorded in BatchStats.Failures.
func FetchWorkloadInputsBatch(
	ctx context.Context,
	pc *promclient.Client,
	cands []promclient.ShardCandidate,
	rsCfg sustainv1alpha1.ResourcesConfigs,
	maxSamples int,
) (BatchInputs, BatchStats) {
	logger := log.FromContext(ctx)

	out := make(BatchInputs, len(cands))
	for _, c := range cands {
		out[c.Identity] = &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{},
			MemPerPod: promclient.ContainerValues{},
		}
	}

	cpuQuantile := PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := ResourceWindow(rsCfg.CPU.Window)
	memQuantile := PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := ResourceWindow(rsCfg.Memory.Window)

	cpuShards, dropped := promclient.BuildShards(cands, promclient.WindowMinutes(cpuWindow), maxSamples)
	// Both BuildShards calls drop the same candidates, so log dropped once.
	memShards, _ := promclient.BuildShards(cands, promclient.WindowMinutes(memWindow), maxSamples)

	for _, d := range dropped {
		logger.V(1).Info("shard candidate dropped: malformed identity, no recommendation this cycle",
			"namespace", d.Identity.Namespace, "ownerKind", d.Identity.OwnerKind, "ownerName", d.Identity.OwnerName)
	}

	// mu guards out, fallenBack and failures inside the per-shard goroutines.
	var mu sync.Mutex
	// fallenBack prevents a second fallback when both shards of an identity fail.
	fallenBack := make(map[promclient.WorkloadIdentity]bool)
	failures := make(map[promclient.WorkloadIdentity]error)

	// The three passes run sequentially, so fallenBack only sees concurrent
	// access within one pass. OOM reuses the CPU partition: its recording
	// rules are pre-aggregated, so shard size does not depend on the window.
	fetchShardedCPU(ctx, pc, cpuShards, cpuQuantile, cpuWindow, rsCfg, out, &mu, logger, fallenBack, failures)
	fetchShardedMemory(ctx, pc, memShards, memQuantile, memWindow, rsCfg, out, &mu, logger, fallenBack, failures)
	fetchShardedOOM(ctx, pc, cpuShards, out, &mu, logger)

	return out, BatchStats{Failures: failures}
}

// fetchShardedCPU queries each shard's CPU data concurrently. A shard that
// fails twice falls back to per-workload queries.
func fetchShardedCPU(
	ctx context.Context,
	pc *promclient.Client,
	shards []promclient.Shard,
	quantile float64,
	window string,
	rsCfg sustainv1alpha1.ResourcesConfigs,
	out BatchInputs,
	mu *sync.Mutex,
	logger logr.Logger,
	fallenBack map[promclient.WorkloadIdentity]bool,
	failures map[promclient.WorkloadIdentity]error,
) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(shardFetchConcurrency)
	for _, shard := range shards {
		g.Go(func() error {
			vals, err := pc.QueryShardCPU(gctx, shard, quantile, window)
			if err != nil {
				vals, err = pc.QueryShardCPU(gctx, shard, quantile, window)
			}
			if err != nil {
				logger.V(1).Info("cpu shard query failed after retry; falling back to per-workload fetch",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", shard.Names, "err", err)
				fallbackShardPerWorkload(gctx, pc, shard, rsCfg, out, mu, logger, fallenBack, failures)
				return nil // never cancel sibling shards
			}
			mu.Lock()
			for id, cv := range vals {
				if wi, ok := out[id]; ok {
					wi.CPUPerPod = cv
				}
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}

// fetchShardedMemory is fetchShardedCPU's memory counterpart.
func fetchShardedMemory(
	ctx context.Context,
	pc *promclient.Client,
	shards []promclient.Shard,
	quantile float64,
	window string,
	rsCfg sustainv1alpha1.ResourcesConfigs,
	out BatchInputs,
	mu *sync.Mutex,
	logger logr.Logger,
	fallenBack map[promclient.WorkloadIdentity]bool,
	failures map[promclient.WorkloadIdentity]error,
) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(shardFetchConcurrency)
	for _, shard := range shards {
		g.Go(func() error {
			vals, err := pc.QueryShardMemory(gctx, shard, quantile, window)
			if err != nil {
				vals, err = pc.QueryShardMemory(gctx, shard, quantile, window)
			}
			if err != nil {
				logger.V(1).Info("memory shard query failed after retry; falling back to per-workload fetch",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", shard.Names, "err", err)
				fallbackShardPerWorkload(gctx, pc, shard, rsCfg, out, mu, logger, fallenBack, failures)
				return nil // never cancel sibling shards
			}
			mu.Lock()
			for id, cv := range vals {
				if wi, ok := out[id]; ok {
					wi.MemPerPod = cv
				}
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}

// fetchShardedOOM queries each shard's OOM signal concurrently. OOM is
// best-effort: a shard that fails twice is logged and skipped, no fallback.
func fetchShardedOOM(
	ctx context.Context,
	pc *promclient.Client,
	shards []promclient.Shard,
	out BatchInputs,
	mu *sync.Mutex,
	logger logr.Logger,
) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(shardFetchConcurrency)
	for _, shard := range shards {
		g.Go(func() error {
			sigs, err := pc.QueryShardOOMSignal(gctx, shard)
			if err != nil {
				sigs, err = pc.QueryShardOOMSignal(gctx, shard)
			}
			if err != nil {
				logger.V(1).Info("oom shard query failed after retry; proceeding without oom floor for this shard",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", shard.Names, "err", err)
				return nil // best-effort, no fallback
			}
			mu.Lock()
			for id, sig := range sigs {
				if wi, ok := out[id]; ok {
					wi.OOM = sig
				}
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}

// fallbackShardPerWorkload re-fetches every name in shard individually via
// FetchWorkloadInputs and merges the result field by field, so no field's
// value depends on pass ordering. Names are used raw: FetchWorkloadInputs
// builds an exact-match selector, and an RE2-escaped name would not match.
func fallbackShardPerWorkload(
	ctx context.Context,
	pc *promclient.Client,
	shard promclient.Shard,
	rsCfg sustainv1alpha1.ResourcesConfigs,
	out BatchInputs,
	mu *sync.Mutex,
	logger logr.Logger,
	fallenBack map[promclient.WorkloadIdentity]bool,
	failures map[promclient.WorkloadIdentity]error,
) {
	// Claims happen serially on this goroutine; only the network calls fan out.
	g := new(errgroup.Group)
	g.SetLimit(fallbackFetchConcurrency)

	for _, name := range shard.Names {
		id := promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}

		mu.Lock()
		if fallenBack[id] {
			mu.Unlock()
			continue
		}
		wi, ok := out[id]
		if !ok {
			// shard.Names derives from cands, which seeded out; guard anyway.
			mu.Unlock()
			continue
		}
		// Claim before querying: a failed fallback would fail identically if retried.
		fallenBack[id] = true
		mu.Unlock()

		g.Go(func() error {
			fetched, err := FetchWorkloadInputs(ctx, pc, shard.Namespace, shard.OwnerKind, name, rsCfg)
			if err != nil {
				logger.V(1).Info("per-workload fallback query failed; recording as a batch failure for this workload",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "ownerName", name, "err", err)
				mu.Lock()
				failures[id] = err
				mu.Unlock()
				return nil // never cancel sibling fetches
			}

			mu.Lock()
			wi.CPUPerPod = fetched.CPUPerPod
			wi.MemPerPod = fetched.MemPerPod
			wi.OOM = fetched.OOM
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}
