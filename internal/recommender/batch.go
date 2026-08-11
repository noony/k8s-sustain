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

// shardFetchConcurrency bounds how many shards within a single pass (CPU,
// memory, or OOM) are queried concurrently.
//
// This is a GOROUTINE bound, not a Prometheus load bound -- those are
// deliberately separate concerns. The actual ceiling on concurrent Prometheus
// requests is the promclient.Client's own in-flight semaphore
// (--prometheus-max-inflight, default 8; see client.go), which every query
// this package issues already goes through. shardFetchConcurrency only needs
// to be "enough to keep that semaphore saturated without spawning thousands
// of blocked goroutines for a policy with many shards" -- it must not be
// treated as, or relied on as, the authoritative throttle on Prometheus load.
const shardFetchConcurrency = 8

// fallbackFetchConcurrency bounds the per-workload queries issued when a shard
// query fails twice and fallbackShardPerWorkload takes over.
//
// This needs its own bound because a shard is large by design: at the shipped
// --query-shard-max-samples (10M) a 7d single-container shard holds nearly a
// thousand workloads. Run serially, that is a thousand sequential Prometheus
// round trips inside one goroutine -- and the failure this fallback exists for
// (Prometheus rejecting an over-budget query) is DETERMINISTIC, so the retry
// above it fails identically and the fallback is the normal path, not a rare
// one. A slow-but-alive Prometheus never trips the circuit breaker, so that
// serial walk could outlast --reconcile-interval and the 10m sweepGracePeriod
// that recommendation_cache.go documents as a correctness assumption.
//
// Like shardFetchConcurrency this is a goroutine bound, not the Prometheus load
// bound -- the client's in-flight semaphore remains the authoritative throttle,
// and these fetches pass through it like every other query. Kept smaller than
// shardFetchConcurrency because the two multiply: all shards in a pass can be
// falling back at once.
const fallbackFetchConcurrency = 4

// BatchInputs maps each requested workload identity to its fetched inputs.
// FetchWorkloadInputsBatch guarantees an entry for every identity it was
// asked about, so a missing key always means "never requested" -- never
// "queried, found nothing" (that case is a present, non-nil *WorkloadInputs
// with empty CPUPerPod/MemPerPod maps).
type BatchInputs map[promclient.WorkloadIdentity]*WorkloadInputs

// BatchStats carries per-identity FETCH OUTCOMES from
// FetchWorkloadInputsBatch -- information BatchInputs cannot express on its
// own, because a present-but-empty *WorkloadInputs is deliberately
// indistinguishable from "queried successfully, no samples" (see BatchInputs'
// doc comment). That ambiguity is fine for the recommendation math (an empty
// percentile input correctly produces no recommendation either way), but it
// is NOT fine for the caller's failure/retry bookkeeping: "this workload is
// too young to have data" and "Prometheus is completely unreachable" must
// not look identical in Policy status, events, and retry state the way they
// would if BatchStats did not exist.
type BatchStats struct {
	// Failures maps an identity to the error from its last failed fetch
	// attempt. Populated ONLY for a genuine failure: a shard query failed,
	// was retried, and its per-workload fallback (fallbackShardPerWorkload)
	// ALSO failed for that specific identity. An identity that fetched
	// successfully -- via its shard OR via a fallback -- and simply has no
	// samples is never present here, even though its BatchInputs entry looks
	// the same (empty CPUPerPod/MemPerPod) as one that failed and kept its
	// pre-seeded zero value. Callers that want the "genuinely failed" signal
	// MUST consult Failures; BatchInputs alone cannot tell them apart.
	//
	// The stored error is whatever the fallback's FetchWorkloadInputs call
	// returned, unwrapped by neither this package nor that one -- in
	// particular promclient.ErrCircuitOpen (the client's circuit breaker
	// signalling a known-down Prometheus) survives here and remains
	// detectable via errors.Is. That is the strongest available signal that
	// something is actually wrong, as opposed to a workload legitimately
	// having nothing to report, and callers should route it through the same
	// failure/retry path a direct FetchWorkloadInputs error would have taken
	// before this package existed.
	//
	// OOM failures never appear here: OOM is best-effort everywhere else in
	// this file (see "OOM is best-effort, never falls back" below) and never
	// denies a workload its CPU/memory recommendation, so an OOM-only
	// failure must not trigger retry/PartialFailure machinery either.
	Failures map[promclient.WorkloadIdentity]error
}

// FetchWorkloadInputsBatch is the sharded counterpart of FetchWorkloadInputs:
// instead of one CPU + one memory + one OOM query PER workload, it groups
// cands into a handful of batched shard queries (internal/prometheus's
// BuildShards/QueryShardCPU/QueryShardMemory/QueryShardOOMSignal) and fans the
// results back out to individual WorkloadInputs. At 10k workloads this is the
// difference between tens of thousands of Prometheus round trips per
// reconcile and a few dozen.
//
// # Why this function returns no error, ever
//
// A single call covers potentially hundreds of workloads sharing a shard. If
// this returned an error, every caller would face an impossible choice: deny
// a recommendation to ALL of them because ONE shard query failed, or ignore
// the error and silently produce a possibly-incomplete map anyway (in which
// case the error was never actionable to begin with). Neither is acceptable
// when the whole point of batching is to serve many healthy workloads that
// happen to share infrastructure with zero unhealthy ones.
//
// Instead, failure is absorbed in layers, each cheaper and more targeted than
// the last:
//
//  1. A shard query (CPU or memory) fails -> retry the shard once. Transient
//     errors (a dropped connection, a momentarily overloaded Prometheus) are
//     the common case and resolve on retry without paying the cost of
//     splitting the shard apart. This one extra attempt is NOT a backoff
//     mechanism and must not grow into one: repeated-failure protection
//     against a genuinely down Prometheus already exists one layer down, in
//     promclient.Client's own circuit breaker (internal/prometheus/breaker.go),
//     which every query this package issues already goes through (trips
//     after 5 consecutive failures, sheds load for a cooldown period). Adding
//     a second, shard-level backoff on top would just fight that one.
//  2. Still fails -> fall back to the existing single-workload
//     FetchWorkloadInputs, once per name in that shard. This is the same code
//     path the webhook and the pre-batching controller already used, so its
//     reliability characteristics are already proven; it is simply more
//     expensive per workload, which is an acceptable price for the rare shard
//     that cannot be served as one query.
//  3. A per-workload fallback query ALSO fails -> that one workload keeps the
//     pre-seeded empty WorkloadInputs (see below) for recommendation-math
//     purposes, but the error is NOT discarded: it is recorded in the
//     returned BatchStats.Failures, so the caller can still distinguish "no
//     data, genuinely nothing to recommend from" from "no data, because
//     Prometheus could not be reached" and route the latter through its
//     normal failure/retry handling. Every other workload in the shard, and
//     every other shard, is completely unaffected.
//
// OOM is handled differently: see "OOM is best-effort, never falls back"
// below.
//
// # Every requested identity gets a non-nil entry
//
// The result map is pre-seeded from cands, with empty (non-nil) CPUPerPod/
// MemPerPod maps and a zero OOMSignal, BEFORE any query runs. Shard and
// fallback results only ever overwrite fields on an existing entry; nothing
// deletes or skips creating one. This holds under every failure mode,
// including a total Prometheus outage: callers can always safely index the
// map for any identity they passed in and distinguish "queried, no data" (a
// present entry with empty maps) from "never asked about" (no entry at all) --
// the same distinction FetchWorkloadInputs's caller gets implicitly from a
// non-nil *WorkloadInputs, now preserved across a batch of many workloads.
//
// # CPU and memory shard independently
//
// rsCfg.CPU.Window and rsCfg.Memory.Window are independent fields and
// frequently differ (memory is usually windowed longer than CPU to avoid
// reacting to transient spikes). Different windows produce different
// WindowMinutes and therefore different BuildShards groupings -- packing a
// 200-workload shard that is safe at CPU's window might be oversized at
// memory's. Two independent shard sets are built and walked separately so
// each resource's sample budget is judged against its own window.
//
// # OOM is best-effort, never falls back
//
// OOM shares CPU's shard partition purely as a name-batching convenience: the
// OOM recording rules are already-aggregated 24h summaries (one or a few
// samples per workload, regardless of rsCfg's windows), so their query cost
// does not scale with window length the way the CPU/memory range queries do
// -- there is no need for a third, independently-sized shard set, and CPU's
// partition is as good as any other for grouping the same namespace/owner_kind
// candidates into name alternations.
//
// An OOM shard failure logs at V(1) and moves on WITHOUT falling back to
// per-workload OOM queries, mirroring FetchWorkloadInputs's own treatment of
// OOM as non-fatal. This is deliberate, not an oversight: OOM data is sparse
// by nature (QueryShardOOMSignal's doc comment -- most workloads never OOM),
// so a failed OOM shard denies nothing but an optional memory floor that the
// overwhelming majority of workloads in the shard would never have used
// anyway. Falling back per-workload here would multiply round trips for a
// signal that rarely carries data, defeating the batching this function
// exists to provide. (A CPU or memory shard failure DOES still trigger a
// per-workload fallback via FetchWorkloadInputs, which incidentally re-fetches
// OOM too for those specific names -- that is a side effect of reusing the
// existing single-workload function for that fallback, not a second OOM
// fallback path.)
//
// # Concurrency
//
// Shards WITHIN a single pass (all CPU shards, then all memory shards, then
// all OOM shards) run concurrently, bounded by shardFetchConcurrency. The
// three passes themselves run strictly SEQUENTIALLY relative to each other --
// CPU fully finishes (including any retries and fallbacks) before memory
// starts, and memory fully finishes before OOM starts. This is what lets
// fallenBack (see fallbackShardPerWorkload) stay a single shared map guarded
// by one mutex instead of needing cross-pass coordination: by the time a
// later pass runs, every earlier pass's goroutines have already been joined
// via g.Wait(), giving a clear happens-before edge -- concurrent access only
// ever happens within one pass, never across passes.
//
// A shard failing, even after its retry, never cancels its siblings: each
// per-shard goroutine always returns nil to the errgroup, exactly like
// PolicyReconciler's per-workload fan-out in policy_controller.go. Errors are
// handled entirely inside the goroutine (retry, then fallback, then log and
// move on) -- there is nothing left for the group to propagate.
//
// # out and fallenBack are shared mutable state -- always accessed under mu
//
// Every read or write of the out map's contents, or of the fallenBack map,
// from inside a per-shard goroutine goes through the shared mutex. Two
// concurrently-running shards in the same pass are constructed to touch
// disjoint sets of workload identities (BuildShards partitions candidates so
// no identity appears in two shards), so in practice these goroutines are not
// contending for the same *WorkloadInputs entry. But Go maps are not merely
// "unsafe to race on shared data" -- concurrent access to a map from multiple
// goroutines without synchronization is undefined behavior even when the
// touched keys are disjoint, and can crash the process outright ("fatal
// error: concurrent map writes") rather than just trip the race detector.
// "Disjoint in practice" is an invariant of BuildShards, not something this
// package can see or enforce locally, so every map touch is mutex-guarded
// regardless.
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
	// BuildShards' drop criteria (empty namespace/owner_kind/owner_name) does
	// not depend on windowMinutes at all -- see its doc comment -- so building
	// the memory shard set a second time from the same cands is guaranteed to
	// drop exactly the same candidates. Logging the CPU call's dropped slice
	// once is therefore complete; a second pass over memShardsDropped would
	// only ever repeat the same log lines.
	memShards, _ := promclient.BuildShards(cands, promclient.WindowMinutes(memWindow), maxSamples)

	for _, d := range dropped {
		logger.V(1).Info("shard candidate dropped: malformed identity, no recommendation this cycle",
			"namespace", d.Identity.Namespace, "ownerKind", d.Identity.OwnerKind, "ownerName", d.Identity.OwnerName)
	}

	// mu guards every access to out's contents, to fallenBack, and to
	// failures from inside the per-shard goroutines each pass spawns -- see
	// the "out and fallenBack are shared mutable state" section above (that
	// reasoning applies identically to failures).
	var mu sync.Mutex
	// fallenBack tracks identities already fully re-fetched by
	// fallbackShardPerWorkload during THIS call, so that a workload whose CPU
	// AND memory shards both fail (rare, but the two shard sets are built
	// independently and can genuinely both come up bad) is re-fetched once,
	// not twice. FetchWorkloadInputs already returns CPU+memory+OOM together,
	// so the first fallback fully satisfies both resources' needs for that
	// identity -- a second fallback pass would just repeat the same query.
	fallenBack := make(map[promclient.WorkloadIdentity]bool)
	// failures collects BatchStats.Failures as fallbackShardPerWorkload's
	// per-identity fetches come back -- see BatchStats' doc comment for what
	// belongs here (and, just as importantly, what does not: a successful
	// empty result is never recorded).
	failures := make(map[promclient.WorkloadIdentity]error)

	fetchShardedCPU(ctx, pc, cpuShards, cpuQuantile, cpuWindow, rsCfg, out, &mu, logger, fallenBack, failures)
	fetchShardedMemory(ctx, pc, memShards, memQuantile, memWindow, rsCfg, out, &mu, logger, fallenBack, failures)
	// Reuses cpuShards for grouping -- see the OOM section of the doc comment
	// above for why a third shard set is unnecessary.
	fetchShardedOOM(ctx, pc, cpuShards, out, &mu, logger)

	return out, BatchStats{Failures: failures}
}

// fetchShardedCPU queries every shard's CPU data concurrently (bounded by
// shardFetchConcurrency), writing each identity's CPUPerPod from a successful
// QueryShardCPU. A shard that fails twice (original + one retry) falls back
// to per-workload queries for every name it contains. No shard's failure
// cancels any other shard's goroutine -- see FetchWorkloadInputsBatch's
// "Concurrency" doc section.
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
				return nil // never cancel sibling shard goroutines
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
	_ = g.Wait() // goroutines always return nil; failures are handled inside each
}

// fetchShardedMemory is fetchShardedCPU's memory counterpart. It shares the
// fallenBack set (and mutex) with fetchShardedCPU, but the two passes never
// run concurrently with each other -- see FetchWorkloadInputsBatch's
// "Concurrency" doc section for why that keeps fallenBack's bookkeeping
// correct without needing cross-pass coordination.
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
				return nil // never cancel sibling shard goroutines
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
	_ = g.Wait() // goroutines always return nil; failures are handled inside each
}

// fetchShardedOOM queries every shard's OOM signal concurrently, writing each
// identity's OOM field from a successful QueryShardOOMSignal. A shard that
// fails twice logs and moves on WITHOUT a per-workload fallback -- see
// FetchWorkloadInputsBatch's doc comment for why OOM is deliberately excluded
// from the fallback tier that CPU and memory get. Identities absent from a
// successful response (the sparse-map contract -- most workloads never OOM)
// are left with their pre-seeded zero OOMSignal, which is the correct "no
// OOM" representation, not an error.
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
				return nil // best-effort: no fallback, see doc comment above
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
	_ = g.Wait() // goroutines always return nil; failures are handled inside each
}

// fallbackShardPerWorkload re-fetches every name in shard individually via
// the existing single-workload FetchWorkloadInputs, merging the result into
// that identity's entry in out FIELD BY FIELD (CPUPerPod, MemPerPod, OOM
// individually) -- deliberately NOT a wholesale `*wi = *fetched` struct
// replace.
//
// The field-level merge matters because correctness must not depend on which
// pass happens to run last. A wholesale replace would make OOM data's safety
// an invisible side effect of the current CPU-then-memory-then-OOM pass
// ordering: today nothing can clobber wi.OOM after fetchShardedOOM runs last,
// but that protection would exist only because of where this file happens to
// call the three fetchSharded* functions. Reorder them, run them concurrently
// with each other instead of sequentially, or add a fourth pass later, and a
// CPU or memory fallback firing after OOM already wrote real data would
// silently overwrite that OOM signal with whatever OOM value
// FetchWorkloadInputs's own (unrelated) fetch happened to return. Assigning
// field-by-field makes each field's value depend only on "the most recent
// successful fetch OF THAT FIELD", never on unrelated fields the same fetch
// call happened to also return.
//
// This does mean a fallback overwrites CPUPerPod/MemPerPod even when that
// specific field's shard query already succeeded -- e.g. only the memory
// shard failed for this identity, so this function's FetchWorkloadInputs call
// re-fetches and rewrites an already-correct CPUPerPod too. That is a
// deliberate simplification, not an oversight: tracking "which fields are
// already correct per identity" would add real bookkeeping to save one query
// on an already-rare failure path, and the re-fetched value is numerically
// identical to the batched one (same recording rule, same window) -- the
// only cost is a wasted round trip, never incorrect data.
//
// It reconstructs WorkloadIdentity from shard.Namespace/OwnerKind and the RAW
// names in shard.Names. Names must NOT be escaped here: FetchWorkloadInputs
// builds an exact-match `owner_name="..."` selector (workloadSelector), which
// needs the literal Kubernetes object name -- an RE2-escaped form (as used
// only inside Shard.Selector for the batched alternation) would not match
// anything.
//
// fallenBack is shared across both the CPU and memory callers: an identity
// already fully fetched by an earlier call (e.g. its CPU shard failed and was
// handled first) is skipped here even though its memory shard also failed,
// because FetchWorkloadInputs already returned memory data for it too. All
// reads and writes of fallenBack and of out go through mu -- see
// FetchWorkloadInputsBatch's "out and fallenBack are shared mutable state"
// doc section for why this holds even though the touched identities are
// expected to be disjoint across concurrently-running shards.
//
// A per-workload query failure for one name is NOT silently swallowed
// anymore: the identity's BatchInputs entry keeps whatever it already had
// (the pre-seeded empty WorkloadInputs if nothing else wrote to it yet, so
// the recommendation math still degrades gracefully), but the error itself is
// recorded in failures so the caller can tell "genuinely failed" apart from
// "queried fine, nothing to report" -- see BatchStats' doc comment. Either
// way, this workload is skipped for a recommendation this cycle; it never
// blocks or drops any other name in the shard.
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
	// Claiming happens on this goroutine, serially, before anything is
	// dispatched: fallenBack's bookkeeping is unchanged from the serial version,
	// and only the network calls move off it. The queries themselves run
	// concurrently under fallbackFetchConcurrency -- a shard can hold close to a
	// thousand workloads, and walking that serially is what could outlast the
	// reconcile interval.
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
			// Cannot happen in practice: shard.Names is derived from cands,
			// which is exactly what seeded out. Guarded defensively anyway
			// rather than risking a nil-map write.
			mu.Unlock()
			continue
		}
		// Marked attempted regardless of outcome, before the query even
		// runs: a failed fallback fetch would fail identically on a second
		// call from the other resource's pass, so there is nothing to gain
		// by retrying it here too.
		fallenBack[id] = true
		mu.Unlock()

		// The network call itself runs OUTSIDE the lock: it is the expensive
		// part, and holding mu across it would serialize every fallback in
		// this shard behind Prometheus round-trip latency for no reason --
		// the fallenBack claim above already prevents duplicate work on this
		// identity.
		g.Go(func() error {
			fetched, err := FetchWorkloadInputs(ctx, pc, shard.Namespace, shard.OwnerKind, name, rsCfg)
			if err != nil {
				logger.V(1).Info("per-workload fallback query failed; recording as a batch failure for this workload",
					"namespace", shard.Namespace, "ownerKind", shard.OwnerKind, "ownerName", name, "err", err)
				mu.Lock()
				failures[id] = err
				mu.Unlock()
				// nil: one workload's failure must not cancel its siblings,
				// exactly as in the shard passes above.
				return nil
			}

			mu.Lock()
			wi.CPUPerPod = fetched.CPUPerPod
			wi.MemPerPod = fetched.MemPerPod
			wi.OOM = fetched.OOM
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines always return nil; failures are recorded in `failures`
}
