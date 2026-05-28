package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
	autoInfo autoscaler.Info,
	workloadCreated time.Time,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs
	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)

	inputs, err := recommender.FetchWorkloadInputs(ctx, r.PrometheusClient, ns, ownerKind, ownerName, rsCfg)
	if err != nil {
		return nil, err
	}

	var liveOOMs map[string]*oomwatch.OOMRecord
	if r.LiveOOM.Enabled() {
		liveOOMs = r.LiveOOM.Source.RecentByWorkload(ns, ownerKind, ownerName, r.LiveOOM.EffectiveMaxAge())
	}
	recentOOM := inputs.HasRecentOOM() || len(liveOOMs) > 0

	// Skip recommendation when the workload itself is too young to have
	// produced stable rate samples. This is a workload-age question, not a
	// sample-count question — the latter punishes workloads with intrinsically
	// sparse signal (e.g. a daily CronJob), since percentile queries handle
	// absent samples correctly but a count-based gate sees the same sparsity
	// as "no history". EXCEPTION: a recent OOM bypasses the gate so a
	// crash-looping container can still get a memory recommendation from the
	// OOM peak below.
	if recommender.ShouldSkipYoungWorkload(workloadCreated, recentOOM) {
		recommendationSkipped.WithLabelValues(ns, ownerKind, ownerName, "workload_too_young").Inc()
		logger.Info("skipping recommendation: workload too young",
			"age", time.Since(workloadCreated), "minAge", recommender.MinWorkloadAge)
		return map[string]workload.ContainerRecommendation{}, nil
	}

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination
	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		cpuPerPod, hasCPU := inputs.CPUPerPod[c.Name]
		memPerPod, hasMem := inputs.MemPerPod[c.Name]
		_, hasPeak := inputs.OOM.PeakMemoryBytes[c.Name]
		liveRec := liveOOMs[c.Name]

		// Construct the per-container OOM context: prometheus signal + any
		// live OOM observation, with a fallback to the cache-captured cgroup
		// limit when Prometheus hasn't yet surfaced it.
		oom := recommender.NewOOMSignal(recentOOM, inputs.OOM.PeakMemoryBytes[c.Name], inputs.OOM.OOMLimitBytes[c.Name])
		if liveRec != nil {
			oom.LiveEventAt = liveRec.TerminatedAt
			if oom.OOMTimeLimitBytes == 0 && liveRec.OOMLimitBytes > 0 {
				oom.OOMTimeLimitBytes = float64(liveRec.OOMLimitBytes)
			}
		}

		res := recommender.ComputeContainerRec(recommender.ContainerInputs{
			Container:   c,
			CPUPerPod:   cpuPerPod,
			HasCPU:      hasCPU,
			MemPerPod:   memPerPod,
			HasMemUsage: hasMem,
			OOM:         oom,
			HasOOMPeak:  hasPeak,
			AutoInfo:    autoInfo,
			RsCfg:       rsCfg,
			CoordCfg:    coordCfg,
		})
		if !res.HasData {
			continue
		}
		if res.MemFloorApplied {
			oomFloorApplied.WithLabelValues(ns, ownerKind, ownerName, c.Name).Inc()
			if liveRec != nil && !liveRec.TerminatedAt.IsZero() {
				EmitOOMReactionLatency(ns, ownerKind, ownerName, time.Since(liveRec.TerminatedAt).Seconds())
			}
		}
		emitCoordinationFactors(ns, ownerKind, ownerName, coordCfg, autoInfo, res.Base, res.Rec)

		recs[c.Name] = res.Rec
	}
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

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return "<nil>"
	}
	return q.String()
}
