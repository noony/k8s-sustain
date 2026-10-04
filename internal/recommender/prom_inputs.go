package recommender

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// PromInputs is the InputsFetcher backed by Prometheus. It packs identities
// into sharded batch queries and retries a failed shard once. A shard that
// still fails is re-queried one identity at a time, so one sick shard cannot
// deny its healthy shard-mates their inputs (docs/adr/0001).
type PromInputs struct {
	client     *promclient.Client
	maxSamples int
}

var _ InputsFetcher = (*PromInputs)(nil)

// NewPromInputs returns a PromInputs whose shards stay within maxSamples,
// counted as containers times window minutes. A non-positive budget puts every
// identity in a shard of its own.
func NewPromInputs(c *promclient.Client, maxSamples int) *PromInputs {
	return &PromInputs{client: c, maxSamples: maxSamples}
}

// FetchInputs fetches CPU, memory and OOM inputs, one signal after the other.
// CPU and memory are required: an identity whose query fails even on its own
// gets that error. OOM is best-effort: a failure leaves an empty OOM signal.
func (p *PromInputs) FetchInputs(
	ctx context.Context,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
) map[promclient.WorkloadIdentity]InputsResult {
	logger := log.FromContext(ctx)

	cpuQuantile := PercentileQuantile(cfg.CPU.Requests.Percentile)
	cpuWindow := ResourceWindow(cfg.CPU.Window)
	memQuantile := PercentileQuantile(cfg.Memory.Requests.Percentile)
	memWindow := ResourceWindow(cfg.Memory.Window)

	cpuShards, dropped := p.plan(reqs, cpuWindow)
	memShards, _ := p.plan(reqs, memWindow)
	for _, id := range dropped {
		logger.V(1).Info("identity with an empty namespace, kind or name cannot be queried; treating it as having no samples",
			"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName)
	}

	cpu, cpuErrs := fetchSignal(ctx, "cpu", cpuShards,
		func(ctx context.Context, s promclient.Shard) (map[promclient.WorkloadIdentity]promclient.ContainerValues, error) {
			return p.client.QueryShardCPU(ctx, s, cpuQuantile, cpuWindow)
		})
	mem, memErrs := fetchSignal(ctx, "memory", memShards,
		func(ctx context.Context, s promclient.Shard) (map[promclient.WorkloadIdentity]promclient.ContainerValues, error) {
			return p.client.QueryShardMemory(ctx, s, memQuantile, memWindow)
		})
	// OOM reuses the CPU partition: its recording rules are pre-aggregated, so
	// shard cost does not depend on the window.
	oom, oomErrs := fetchSignal(ctx, "oom", cpuShards, p.client.QueryShardOOMSignal)

	out := make(map[promclient.WorkloadIdentity]InputsResult, len(reqs))
	for _, r := range reqs {
		id := r.Identity
		if err := cpuErrs[id]; err != nil {
			out[id] = InputsResult{Err: fmt.Errorf("cpu inputs: %w", err)}
			continue
		}
		if err := memErrs[id]; err != nil {
			out[id] = InputsResult{Err: fmt.Errorf("memory inputs: %w", err)}
			continue
		}
		if err := oomErrs[id]; err != nil {
			logger.V(1).Info("oom inputs unavailable; proceeding without the OOM floor",
				"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName, "err", err)
		}
		out[id] = InputsResult{Inputs: &WorkloadInputs{
			CPUPerPod: nonNil(cpu[id]),
			MemPerPod: nonNil(mem[id]),
			OOM:       oom[id],
		}}
	}
	return out
}

// plan partitions reqs into shards for one window. An identity of unknown
// size gets a shard of its own, since packing it blind could push a shard past
// the sample budget. Malformed identities are returned in dropped.
func (p *PromInputs) plan(reqs []InputsRequest, window string) (shards []promclient.Shard, dropped []promclient.WorkloadIdentity) {
	minutes := promclient.WindowMinutes(window)
	seen := make(map[promclient.WorkloadIdentity]bool, len(reqs))
	var sized []promclient.ShardCandidate
	var unsized []promclient.ShardCandidate
	for _, r := range reqs {
		if seen[r.Identity] {
			continue
		}
		seen[r.Identity] = true
		c := promclient.ShardCandidate{Identity: r.Identity, Containers: r.Containers}
		if r.Containers > 0 {
			sized = append(sized, c)
		} else {
			unsized = append(unsized, c)
		}
	}

	collect := func(cands []promclient.ShardCandidate) {
		s, d := promclient.BuildShards(cands, minutes, p.maxSamples)
		shards = append(shards, s...)
		for _, c := range d {
			dropped = append(dropped, c.Identity)
		}
	}
	collect(sized)
	for _, c := range unsized {
		collect([]promclient.ShardCandidate{c})
	}
	return shards, dropped
}

// fetchSignal runs query over every shard, retrying each once. A shard that
// still fails is re-queried as one-identity shards, each retried once too, so
// errs holds only identities whose query failed on its own.
func fetchSignal[V any](
	ctx context.Context,
	what string,
	shards []promclient.Shard,
	query func(context.Context, promclient.Shard) (map[promclient.WorkloadIdentity]V, error),
) (vals map[promclient.WorkloadIdentity]V, errs map[promclient.WorkloadIdentity]error) {
	logger := log.FromContext(ctx)
	vals = make(map[promclient.WorkloadIdentity]V)
	errs = make(map[promclient.WorkloadIdentity]error)
	var mu sync.Mutex

	run := func(shard promclient.Shard) error {
		got, err := query(ctx, shard)
		if err != nil && ctx.Err() == nil {
			got, err = query(ctx, shard)
		}
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, name := range shard.Names {
			id := promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}
			if v, ok := got[id]; ok {
				vals[id] = v
			}
		}
		return nil
	}
	fail := func(id promclient.WorkloadIdentity, err error) {
		logger.V(1).Info(what+" query failed after retry",
			"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName, "err", err)
		mu.Lock()
		errs[id] = err
		mu.Unlock()
	}

	var g errgroup.Group
	g.SetLimit(shardFetchConcurrency)
	for _, shard := range shards {
		g.Go(func() error {
			err := run(shard)
			if err == nil {
				return nil
			}
			// A cancelled context fails every re-query too, so shutdown does not
			// fan a doomed shard out into one query per identity.
			if len(shard.Names) == 1 || ctx.Err() != nil {
				for _, name := range shard.Names {
					fail(promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}, err)
				}
				return nil
			}
			logger.V(1).Info(what+" shard query failed after retry; re-querying its identities one at a time",
				"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", len(shard.Names), "err", err)
			var fg errgroup.Group
			fg.SetLimit(fallbackFetchConcurrency)
			for _, name := range shard.Names {
				fg.Go(func() error {
					single := promclient.Shard{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, Names: []string{name}}
					if err := run(single); err != nil {
						fail(promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}, err)
					}
					return nil
				})
			}
			_ = fg.Wait()
			return nil
		})
	}
	_ = g.Wait()
	return vals, errs
}

func nonNil(v promclient.ContainerValues) promclient.ContainerValues {
	if v == nil {
		return promclient.ContainerValues{}
	}
	return v
}
