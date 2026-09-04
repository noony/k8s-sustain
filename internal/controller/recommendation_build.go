package controller

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// errPrefetchMissingForBatchedKind is logged, never returned: the fallback
// query still runs and usually succeeds, which is exactly why a desync between
// this guard and Reconcile's candidate-building loop would otherwise be
// invisible while silently degrading back to a per-workload query. The only
// legitimate source of a nil inputs is that loop's own pendingSnapshot
// bookkeeping.
var errPrefetchMissingForBatchedKind = errors.New("prefetched batch inputs missing for an identity that should always be batched")

// recDeps is the slice of reconciler state the recommendation pipeline
// actually needs. It keeps the pipeline callable as a plain function — the
// live-target path (computeIdentity) and the departed path
// (refreshDepartedRecommendation) both drive it, and they write the same
// WorkloadRecommendation objects, so any divergence here would show up as a
// workload whose recommendation changes depending on which path computed it.
type recDeps struct {
	Prom    *promclient.Client
	LiveOOM LiveOOMConfig
}

func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
	autoInfo autoscaler.Info,
	workloadCreated, identityFirstSeen time.Time,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
	snapshotPending bool,
) (map[string]workload.ContainerRecommendation, error) {
	return buildRecommendations(ctx, recDeps{Prom: r.PrometheusClient, LiveOOM: r.LiveOOM},
		policy, ns, ownerKind, ownerName, containers, autoInfo, workloadCreated, identityFirstSeen, inputs, fetchErr, snapshotPending)
}

func buildRecommendations(
	ctx context.Context,
	deps recDeps,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
	autoInfo autoscaler.Info,
	workloadCreated, identityFirstSeen time.Time,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
	// snapshotPending is true when Reconcile's candidate-building loop
	// (policy_controller.go's pendingSnapshot) withheld this identity from the
	// batch for lack of an observed-resources snapshot to size a shard with --
	// an expected first-cycle state, not a bug, so it suppresses
	// errPrefetchMissingForBatchedKind below. Callers not exercising that
	// withholding pass false.
	snapshotPending bool,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs
	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)

	// fetchErr is non-nil only when the batch prefetch genuinely failed to
	// reach Prometheus for this identity: the shard query failed, was retried,
	// and its per-workload fallback failed too (recommender.BatchStats). It is
	// checked before inputs because a total outage otherwise flows through as
	// an empty-but-non-nil inputs indistinguishable from "no data yet" --
	// Ready, no retry, no event, just a V(1) log.
	if fetchErr != nil {
		return nil, fetchErr
	}

	// nil means this identity was not prefetched -- NOT "queried, found
	// nothing" (the batch guarantees a non-nil entry for every candidate it was
	// given, see BatchInputs). The one legitimate cause is snapshotPending; any
	// other nil is a candidate-building bug, logged loudly below so it cannot
	// hide behind a still-successful fallback.
	if inputs == nil {
		if !snapshotPending {
			logger.Error(errPrefetchMissingForBatchedKind,
				"falling back to a per-workload Prometheus query; this defeats batching and should be investigated in Reconcile's candidate-building loop")
		}
		var err error
		inputs, err = recommender.FetchWorkloadInputs(ctx, deps.Prom, ns, ownerKind, ownerName, rsCfg)
		if err != nil {
			return nil, err
		}
	}

	var liveOOMs map[string]*oomwatch.OOMRecord
	if deps.LiveOOM.Enabled() {
		liveOOMs = deps.LiveOOM.Source.RecentByWorkload(ns, ownerKind, ownerName, deps.LiveOOM.EffectiveMaxAge())
	}
	// Workload-level recency: only feeds the age-gate bypass below. The
	// per-container memory floor uses per-container recency (OOMCounts /
	// LiveEventAt) inside BuildContainerRecs, so a sibling's OOM never floors
	// an innocent container.
	recentOOM := inputs.HasRecentOOM() || len(liveOOMs) > 0

	// Skip recommendation when the workload itself is too young to have
	// produced stable rate samples. This is a workload-age question, not a
	// sample-count question — the latter punishes workloads with intrinsically
	// sparse signal (e.g. a daily CronJob), since percentile queries handle
	// absent samples correctly but a count-based gate sees the same sparsity
	// as "no history". The only exception is a recent OOM, which bypasses the
	// gate so a crash-looping container can still get a memory recommendation
	// from the OOM peak below.
	//
	// "Pod"-kind targets (bare pods) are gated like every other kind: since
	// Ongoing bare pods are resized in place (see resizeBarePods), a brand-new
	// identity with partial warm-up samples could otherwise floor to the hard
	// minimum and be applied in place — which for memory can kill the
	// container. The gate keys on the EARLIEST of the pod's creation time and
	// the identity's first-seen time, so a recurring identity still clears it
	// on its long-lived WorkloadRecommendation.
	if recommender.ShouldSkipYoungWorkload(workloadCreated, identityFirstSeen, recentOOM) {
		recommendationSkipped.WithLabelValues(ns, ownerKind, ownerName, "workload_too_young").Inc()
		logger.Info("skipping recommendation: workload too young",
			"age", recommender.AgeForLog(workloadCreated), "identityAge", recommender.AgeForLog(identityFirstSeen), "minAge", recommender.MinWorkloadAge)
		return map[string]workload.ContainerRecommendation{}, nil
	}

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination
	// liveRec is the live-OOM record for the container currently being built.
	// BuildContainerRecs invokes EnrichOOM then OnResult for the same container
	// in sequence (no concurrency), so EnrichOOM looks the record up once and
	// OnResult reuses it — avoiding a second map lookup per container.
	var liveRec *oomwatch.OOMRecord
	recs := recommender.BuildContainerRecs(containers, inputs, autoInfo, rsCfg, coordCfg,
		recommender.BuildContainerRecsOptions{
			// Anchor on whichever source reports the HIGHER OOM-time limit.
			// The two have complementary blind spots and neither can be inflated
			// by k8s-sustain's own resize: Prometheus survives a controller
			// restart but is windowed, so right after a resize-then-OOM it can
			// still report the PREVIOUS limit; the live record has the exact
			// limit applied at that kill but is lost on restart. Preferring
			// Prometheus anchors on the stale, lower limit and under-bumps a
			// container that is still OOM-looping.
			EnrichOOM: func(name string, oom recommender.OOMSignal) recommender.OOMSignal {
				liveRec = liveOOMs[name]
				if liveRec != nil {
					oom.LiveEventAt = liveRec.TerminatedAt
					if live := float64(liveRec.OOMLimitBytes); live > oom.OOMTimeLimitBytes {
						oom.OOMTimeLimitBytes = live
					}
				}
				return oom
			},
			OnResult: func(name string, res recommender.ContainerRecResult) {
				if res.MemFloorApplied {
					oomFloorApplied.WithLabelValues(ns, ownerKind, ownerName, name).Inc()
					if liveRec != nil && !liveRec.TerminatedAt.IsZero() {
						EmitOOMReactionLatency(ns, ownerKind, ownerName, time.Since(liveRec.TerminatedAt).Seconds())
					}
				}
				emitCoordinationFactors(ns, ownerKind, ownerName, coordCfg, autoInfo, res.Base, res.Rec)
			},
		})
	return recs, nil
}

// factorRatio returns adjusted/baseline as a float64. Returns 1.0 (no-op
// signal) when either side is nil or the baseline is zero, so the metric
// never emits NaN/Inf.
func factorRatio(adjusted, baseline *resource.Quantity) float64 {
	if adjusted == nil || baseline == nil || baseline.IsZero() {
		return 1.0
	}
	return float64(adjusted.MilliValue()) / float64(baseline.MilliValue())
}

// emitCoordinationFactors records overhead and (CPU only) replica multipliers
// applied by ApplyCoordination, decomposed for dashboard rendering. No-op when
// coordination is disabled or no autoscaler targets the workload.
func emitCoordinationFactors(
	namespace, ownerKind, ownerName string,
	cfg sustainv1alpha1.AutoscalerCoordination,
	info autoscaler.Info,
	base, adjusted workload.ContainerRecommendation,
) {
	if !cfg.Enabled || info.Kind == autoscaler.KindNone {
		return
	}

	// CPU: overhead-only ratio computed independently so we can split it from
	// the replica correction in the same metric family. Total = overhead × replica.
	if base.CPURequest != nil {
		cpuOverhead := recommender.ApplyOverhead(base.CPURequest, info.ConfiguredTargets[autoscaler.ResourceCPU])
		overheadFactor := factorRatio(cpuOverhead, base.CPURequest)
		EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceCPU, "overhead", overheadFactor)
		if cfg.ReplicaBudgetAnchor != nil {
			totalFactor := factorRatio(adjusted.CPURequest, base.CPURequest)
			replicaFactor := 1.0
			if overheadFactor != 0 {
				replicaFactor = totalFactor / overheadFactor
			}
			EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceCPU, "replica", replicaFactor)
		}
	}

	// Memory: overhead only.
	if base.MemoryRequest != nil {
		EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceMemory, "overhead",
			factorRatio(adjusted.MemoryRequest, base.MemoryRequest))
	}
}
