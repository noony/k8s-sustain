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

// shardFetchConcurrency bounds concurrent shard queries within one signal. It
// is a goroutine bound only; the client's in-flight semaphore throttles
// Prometheus.
const shardFetchConcurrency = 8

// fallbackFetchConcurrency bounds the one-identity re-queries of one failed
// shard. A shard can hold ~1000 identities and the over-budget failure that
// triggers the fallback is deterministic, so a serial walk could outlast the
// reconcile interval. Kept below shardFetchConcurrency because the two
// multiply.
const fallbackFetchConcurrency = 4

// PromInputs is the InputsFetcher backed by Prometheus. It reads every signal,
// packing identities into sharded batch queries sized by that signal's own
// cost and retrying a failed shard once. A shard that still fails is
// re-queried one identity at a time, so one sick shard cannot deny its healthy
// shard-mates their inputs (docs/adr/0001).
type PromInputs struct {
	client     *promclient.Client
	maxSamples int
}

var _ InputsFetcher = (*PromInputs)(nil)

// NewPromInputs returns a PromInputs whose shards stay within maxSamples,
// counted as each signal's samples per container times containers. A
// non-positive budget puts every identity in a shard of its own.
func NewPromInputs(c *promclient.Client, maxSamples int) *PromInputs {
	return &PromInputs{client: c, maxSamples: maxSamples}
}

// FetchInputs fetches every signal, one after the other. An identity whose
// query for a required signal fails even on its own gets that error. A failed
// best-effort signal leaves it holding that signal's empty values.
func (p *PromInputs) FetchInputs(
	ctx context.Context,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
) map[promclient.WorkloadIdentity]InputsResult {
	logger := log.FromContext(ctx)

	inputs := make(map[promclient.WorkloadIdentity]*WorkloadInputs, len(reqs))
	for _, r := range reqs {
		if inputs[r.Identity] != nil {
			continue
		}
		in := &WorkloadInputs{}
		for _, s := range signals {
			s.collect(in, nil)
		}
		inputs[r.Identity] = in
	}

	failed := make(map[promclient.WorkloadIdentity]error)
	dropped := make(map[promclient.WorkloadIdentity]bool)
	for _, s := range signals {
		shards, d := p.plan(reqs, s.samplesPerContainer(cfg))
		for _, id := range d {
			dropped[id] = true
		}
		for id, err := range p.fetchSignal(ctx, s, cfg, shards, inputs) {
			if !s.required() {
				logger.V(1).Info(s.name()+" inputs unavailable; computing without them",
					"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName, "err", err)
				continue
			}
			if failed[id] == nil {
				failed[id] = fmt.Errorf("%s inputs: %w", s.name(), err)
			}
		}
	}
	for id := range dropped {
		logger.V(1).Info("identity with an empty namespace, kind or name cannot be queried; treating it as having no samples",
			"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName)
	}

	out := make(map[promclient.WorkloadIdentity]InputsResult, len(inputs))
	for id, in := range inputs {
		if err := failed[id]; err != nil {
			out[id] = InputsResult{Err: err}
			continue
		}
		out[id] = InputsResult{Inputs: in}
	}
	return out
}

// plan partitions reqs into shards at one cost per container. An identity of
// unknown size gets a shard of its own, since packing it blind could push a
// shard past the sample budget. Malformed identities are returned in dropped.
func (p *PromInputs) plan(reqs []InputsRequest, samplesPerContainer int) (shards []promclient.Shard, dropped []promclient.WorkloadIdentity) {
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
		s, d := promclient.BuildShards(cands, samplesPerContainer, p.maxSamples)
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

// fetchSignal runs s's query over every shard, retrying each once, and
// collects each answered identity's samples into its inputs. A shard that
// still fails is re-queried as one-identity shards, each retried once too, so
// errs holds only identities whose query failed on its own.
func (p *PromInputs) fetchSignal(
	ctx context.Context,
	s signal,
	cfg sustainv1alpha1.ResourcesConfigs,
	shards []promclient.Shard,
	inputs map[promclient.WorkloadIdentity]*WorkloadInputs,
) (errs map[promclient.WorkloadIdentity]error) {
	logger := log.FromContext(ctx)
	errs = make(map[promclient.WorkloadIdentity]error)
	var mu sync.Mutex

	run := func(shard promclient.Shard) error {
		expr := s.query(cfg, shard)
		samples, err := p.client.QueryShard(ctx, expr)
		if err != nil && ctx.Err() == nil {
			samples, err = p.client.QueryShard(ctx, expr)
		}
		if err != nil {
			return err
		}
		byIdentity := make(map[promclient.WorkloadIdentity][]promclient.ShardSample, len(shard.Names))
		for _, smp := range samples {
			byIdentity[smp.Identity] = append(byIdentity[smp.Identity], smp)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, name := range shard.Names {
			id := promclient.WorkloadIdentity{Namespace: shard.Namespace, OwnerKind: shard.OwnerKind, OwnerName: name}
			s.collect(inputs[id], byIdentity[id])
		}
		return nil
	}
	fail := func(id promclient.WorkloadIdentity, err error) {
		logger.V(1).Info(s.name()+" query failed after retry",
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
			logger.V(1).Info(s.name()+" shard query failed after retry; re-querying its identities one at a time",
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
	return errs
}
