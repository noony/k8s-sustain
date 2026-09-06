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

	f := &batchFetcher{
		pc: pc, rsCfg: rsCfg, out: out, logger: logger,
		fallenBack: make(map[promclient.WorkloadIdentity]bool),
		failures:   make(map[promclient.WorkloadIdentity]error),
	}

	// The three passes run sequentially, so fallenBack only sees concurrent
	// access within one pass. OOM reuses the CPU partition: its recording
	// rules are pre-aggregated, so shard size does not depend on the window.
	fetchSharded(ctx, f, cpuShards, "cpu",
		func(ctx context.Context, shard promclient.Shard) (promclient.IdentityValues, error) {
			return pc.QueryShardCPU(ctx, shard, cpuQuantile, cpuWindow)
		},
		func(wi *WorkloadInputs, cv promclient.ContainerValues) { wi.CPUPerPod = cv },
		f.fallbackShardPerWorkload)
	fetchSharded(ctx, f, memShards, "memory",
		func(ctx context.Context, shard promclient.Shard) (promclient.IdentityValues, error) {
			return pc.QueryShardMemory(ctx, shard, memQuantile, memWindow)
		},
		func(wi *WorkloadInputs, cv promclient.ContainerValues) { wi.MemPerPod = cv },
		f.fallbackShardPerWorkload)
	// OOM is best-effort: a shard that fails twice is logged and skipped.
	fetchSharded(ctx, f, cpuShards, "oom", pc.QueryShardOOMSignal,
		func(wi *WorkloadInputs, sig promclient.OOMSignal) { wi.OOM = sig },
		nil)

	return out, BatchStats{Failures: f.failures}
}

// batchFetcher is the state shared by one FetchWorkloadInputsBatch pass. mu
// guards out, fallenBack and failures inside the per-shard goroutines.
type batchFetcher struct {
	pc     *promclient.Client
	rsCfg  sustainv1alpha1.ResourcesConfigs
	out    BatchInputs
	logger logr.Logger

	mu sync.Mutex
	// fallenBack prevents a second fallback when both shards of an identity fail.
	fallenBack map[promclient.WorkloadIdentity]bool
	failures   map[promclient.WorkloadIdentity]error
}

// fetchSharded queries every shard concurrently, retrying each once. A shard
// that fails twice goes to onFailure when one is given, otherwise it is logged
// and skipped. Results are stored under mu through store, only for identities
// the batch requested.
func fetchSharded[M ~map[promclient.WorkloadIdentity]V, V any](
	ctx context.Context,
	f *batchFetcher,
	shards []promclient.Shard,
	what string,
	query func(context.Context, promclient.Shard) (M, error),
	store func(*WorkloadInputs, V),
	onFailure func(context.Context, promclient.Shard),
) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(shardFetchConcurrency)
	for _, shard := range shards {
		g.Go(func() error {
			vals, err := query(gctx, shard)
			if err != nil {
				vals, err = query(gctx, shard)
			}
			if err != nil {
				f.logger.V(1).Info(what+" shard query failed after retry",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", shard.Names, "err", err,
					"fallback", onFailure != nil)
				if onFailure != nil {
					onFailure(gctx, shard)
				}
				return nil // never cancel sibling shards
			}
			f.mu.Lock()
			for id, v := range vals {
				if wi, ok := f.out[id]; ok {
					store(wi, v)
				}
			}
			f.mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}

// fallbackShardPerWorkload re-fetches every name in shard individually via
// FetchWorkloadInputs and merges the result field by field, so no field's
// value depends on pass ordering. Names are used raw: FetchWorkloadInputs
// builds an exact-match selector, and an RE2-escaped name would not match.
func (f *batchFetcher) fallbackShardPerWorkload(ctx context.Context, shard promclient.Shard) {
	// Claims happen serially on this goroutine; only the network calls fan out.
	g := new(errgroup.Group)
	g.SetLimit(fallbackFetchConcurrency)

	for _, name := range shard.Names {
		id := promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}

		f.mu.Lock()
		if f.fallenBack[id] {
			f.mu.Unlock()
			continue
		}
		wi, ok := f.out[id]
		if !ok {
			// shard.Names derives from cands, which seeded out; guard anyway.
			f.mu.Unlock()
			continue
		}
		// Claim before querying: a failed fallback would fail identically if retried.
		f.fallenBack[id] = true
		f.mu.Unlock()

		g.Go(func() error {
			fetched, err := FetchWorkloadInputs(ctx, f.pc, shard.Namespace, shard.OwnerKind, name, f.rsCfg)
			if err != nil {
				f.logger.V(1).Info("per-workload fallback query failed; recording as a batch failure for this workload",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "ownerName", name, "err", err)
				f.mu.Lock()
				f.failures[id] = err
				f.mu.Unlock()
				return nil // never cancel sibling fetches
			}

			f.mu.Lock()
			*wi = *fetched
			f.mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil
}
