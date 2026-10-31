package recommender

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

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

// FetchInputs fetches every signal as of at, one after the other. An identity
// whose query for a required signal fails even on its own gets that error. A
// failed best-effort signal leaves it holding that signal's empty values.
func (p *PromInputs) FetchInputs(
	ctx context.Context,
	at time.Time,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
) map[promclient.WorkloadIdentity]InputsResult {
	inputs := make(map[promclient.WorkloadIdentity]*WorkloadInputs, len(reqs))
	for _, r := range reqs {
		if inputs[r.Identity] == nil {
			inputs[r.Identity] = emptyInputs()
		}
	}

	failed := p.fetchSignals(ctx, cfg, reqs, 1, func(s signal, shard promclient.Shard) (func(), error) {
		samples, err := p.client.QueryShard(ctx, s.query(cfg, shard), at)
		if err != nil {
			return nil, err
		}
		byIdentity := make(map[promclient.WorkloadIdentity][]promclient.ShardSample, len(shard.Names))
		for _, smp := range samples {
			byIdentity[smp.Identity] = append(byIdentity[smp.Identity], smp)
		}
		return func() {
			for _, id := range shard.Identities() {
				inputs[id].set(s, s.collect(byIdentity[id]))
			}
		}, nil
	})

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

// FetchHistory fetches every signal over span, one after the other: each
// signal's own shard query, the one FetchInputs runs, run as a range query
// under the same sharding, its cost counted at every instant. A failed signal
// fails an identity's history as FetchInputs fails its inputs. The OOM kills
// are best-effort: a history whose kills cannot be read holds none.
func (p *PromInputs) FetchHistory(
	ctx context.Context,
	span Span,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
) map[promclient.WorkloadIdentity]HistoryResult {
	instants := span.instants()
	points := make(map[promclient.WorkloadIdentity][]*WorkloadInputs, len(reqs))
	for _, r := range reqs {
		if points[r.Identity] != nil {
			continue
		}
		at := make([]*WorkloadInputs, instants)
		for i := range at {
			at[i] = emptyInputs()
		}
		points[r.Identity] = at
	}

	failed := p.fetchSignals(ctx, cfg, reqs, instants, func(s signal, shard promclient.Shard) (func(), error) {
		series, err := p.client.QueryShardRange(ctx, s.query(cfg, shard), span.timeRange(), span.Step)
		if err != nil {
			return nil, err
		}
		byInstant := make(map[promclient.WorkloadIdentity][][]promclient.ShardSample, len(shard.Names))
		for _, sr := range series {
			at := byInstant[sr.Identity]
			if at == nil {
				at = make([][]promclient.ShardSample, instants)
				byInstant[sr.Identity] = at
			}
			for _, pt := range sr.Points {
				if i, ok := span.index(pt.Timestamp); ok {
					at[i] = append(at[i], promclient.ShardSample{Identity: sr.Identity, Container: sr.Container, Metric: sr.Metric, Value: pt.Value})
				}
			}
		}
		return func() {
			for _, id := range shard.Identities() {
				samples := byInstant[id]
				for i, in := range points[id] {
					var at []promclient.ShardSample
					if samples != nil {
						at = samples[i]
					}
					in.set(s, s.collect(at))
				}
			}
		}, nil
	})
	kills := p.fetchKills(ctx, span, reqs)

	out := make(map[promclient.WorkloadIdentity]HistoryResult, len(points))
	for id, at := range points {
		if err := failed[id]; err != nil {
			out[id] = HistoryResult{Err: err}
			continue
		}
		h := &History{points: make([]HistoryPoint, len(at)), kills: kills[id]}
		for i, in := range at {
			h.points[i] = HistoryPoint{At: span.instant(i), Inputs: in}
		}
		out[id] = HistoryResult{History: h}
	}
	return out
}

// killReader is a signal whose range form also reads the OOM kills a chart
// marks.
type killReader interface {
	// killsQuery counts, per container of every identity in shard, the OOM
	// kills in each step of a range stepping by step.
	killsQuery(shard promclient.Shard, step time.Duration) string
}

// fetchKills reads, over span, the OOM kills of every signal that reads them,
// in shards costing one sample per container at every instant. An identity
// whose kills cannot be read even on its own gets none.
func (p *PromInputs) fetchKills(ctx context.Context, span Span, reqs []InputsRequest) map[promclient.WorkloadIdentity][]promclient.OOMKill {
	logger := log.FromContext(ctx)
	kills := make(map[promclient.WorkloadIdentity][]promclient.OOMKill)
	for _, s := range signals {
		k, ok := s.(killReader)
		if !ok {
			continue
		}
		shards, _ := p.plan(reqs, span.instants())
		errs := eachShard(ctx, s.name()+" kills", shards, func(shard promclient.Shard) (func(), error) {
			series, err := p.client.QueryShardRange(ctx, k.killsQuery(shard, span.Step), span.timeRange(), span.Step)
			if err != nil {
				return nil, err
			}
			return func() {
				for _, sr := range series {
					kills[sr.Identity] = append(kills[sr.Identity], promclient.OOMEventsOf(sr.Container, sr.Points)...)
				}
			}, nil
		})
		for id, err := range errs {
			logger.V(1).Info(s.name()+" kills unavailable; the history holds none",
				"namespace", id.Namespace, "ownerKind", id.OwnerKind, "ownerName", id.OwnerName, "err", err)
		}
	}
	for _, k := range kills {
		slices.SortFunc(k, func(a, b promclient.OOMKill) int {
			return cmp.Or(a.Timestamp.Compare(b.Timestamp), strings.Compare(a.Container, b.Container))
		})
	}
	return kills
}

// fetchSignals reads every signal, one after the other, over shards planned
// at its samples per container times points. read queries one shard and
// returns what applies its answer, which runs under a lock. The result holds
// the identities a required signal could not be read for even on their own,
// with the error; a failed best-effort signal is only logged.
func (p *PromInputs) fetchSignals(
	ctx context.Context,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
	points int,
	read func(s signal, shard promclient.Shard) (apply func(), err error),
) map[promclient.WorkloadIdentity]error {
	logger := log.FromContext(ctx)
	failed := make(map[promclient.WorkloadIdentity]error)
	dropped := make(map[promclient.WorkloadIdentity]bool)
	for _, s := range signals {
		shards, d := p.plan(reqs, s.samplesPerContainer(cfg)*points)
		for _, id := range d {
			dropped[id] = true
		}
		errs := eachShard(ctx, s.name(), shards, func(shard promclient.Shard) (func(), error) { return read(s, shard) })
		for id, err := range errs {
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
	return failed
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

// eachShard runs query over every shard, retrying each once, and applies
// each answer under one lock. A shard that still fails is re-queried as
// one-identity shards, each retried once too, so errs holds only identities
// whose query failed on its own. name labels the read in logs.
func eachShard(
	ctx context.Context,
	name string,
	shards []promclient.Shard,
	query func(promclient.Shard) (apply func(), err error),
) (errs map[promclient.WorkloadIdentity]error) {
	logger := log.FromContext(ctx)
	errs = make(map[promclient.WorkloadIdentity]error)
	var mu sync.Mutex

	run := func(shard promclient.Shard) error {
		apply, err := query(shard)
		if err != nil && ctx.Err() == nil {
			apply, err = query(shard)
		}
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		apply()
		return nil
	}
	fail := func(id promclient.WorkloadIdentity, err error) {
		logger.V(1).Info(name+" query failed after retry",
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
				for _, id := range shard.Identities() {
					fail(id, err)
				}
				return nil
			}
			logger.V(1).Info(name+" shard query failed after retry; re-querying its identities one at a time",
				"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "names", len(shard.Names), "err", err)
			var fg errgroup.Group
			fg.SetLimit(fallbackFetchConcurrency)
			for _, id := range shard.Identities() {
				fg.Go(func() error {
					single := promclient.Shard{Namespace: id.Namespace, OwnerKind: id.OwnerKind, Names: []string{id.OwnerName}}
					if err := run(single); err != nil {
						fail(id, err)
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
