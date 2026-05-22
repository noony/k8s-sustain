package controller

import (
	"context"
	"fmt"
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

// minWorkloadAge gates the first recommendation on a workload's age. The CPU
// rate rule needs a few minutes after container start to stabilize;
// recommending before that produces near-zero percentile values that get
// floored to the hard minimum and trigger an immediate recycle on the next
// reconcile. 10 minutes leaves headroom past the longest fallback window
// (5m) used by k8s_sustain:container_cpu_usage:rate1m.
const minWorkloadAge = 10 * time.Minute

func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
	autoInfo autoscaler.Info,
	workloadCreated time.Time,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	cpuQuantile := recommender.PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := recommender.ResourceWindow(rsCfg.CPU.Window)
	memQuantile := recommender.PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := recommender.ResourceWindow(rsCfg.Memory.Window)

	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)
	logger.V(1).Info("querying Prometheus (workload-level)",
		"cpuQuantile", cpuQuantile, "cpuWindow", cpuWindow,
		"memQuantile", memQuantile, "memWindow", memWindow)

	// OOM signal — fetched first so the history gate can bypass for
	// crash-looping workloads that can't accumulate usage samples.
	// Fail-open: missing data must not break the recommendation path.
	oomSignal, err := r.PrometheusClient.QueryWorkloadOOMSignal(ctx, ns, ownerKind, ownerName)
	if err != nil {
		logger.V(1).Info("oom signal query failed; proceeding without OOM floor", "err", err)
		oomSignal = promclient.OOMSignal{}
	}

	var liveOOMs map[string]*oomwatch.OOMRecord
	if r.LiveOOM.Enabled() {
		liveOOMs = r.LiveOOM.Source.RecentByWorkload(ns, ownerKind, ownerName, r.LiveOOM.EffectiveMaxAge())
	}
	recentOOM := oomSignal.OOMCount > 0 || len(liveOOMs) > 0

	// Skip recommendation when the workload itself is too young to have
	// produced stable rate samples. This is a workload-age question, not a
	// sample-count question — the latter punishes workloads with intrinsically
	// sparse signal (e.g. a daily CronJob), since percentile queries handle
	// absent samples correctly but a count-based gate sees the same sparsity
	// as "no history". EXCEPTION: a recent OOM bypasses the gate so a
	// crash-looping container can still get a memory recommendation from the
	// OOM peak below.
	if !recentOOM && !workloadCreated.IsZero() {
		if age := time.Since(workloadCreated); age < minWorkloadAge {
			recommendationSkipped.WithLabelValues(ns, ownerKind, ownerName, "workload_too_young").Inc()
			logger.Info("skipping recommendation: workload too young", "age", age, "minAge", minWorkloadAge)
			return map[string]workload.ContainerRecommendation{}, nil
		}
	}

	cpuTotals, err := r.PrometheusClient.QueryWorkloadCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("workload cpu query: %w", err)
	}
	memTotals, err := r.PrometheusClient.QueryWorkloadMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("workload memory query: %w", err)
	}

	// Per-pod p95 floors used for hot-replica protection. A failure here is
	// non-fatal: we still produce recommendations from the workload-level data.
	cpuFloors, err := r.PrometheusClient.QueryCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		logger.V(1).Info("per-pod cpu floor query failed; proceeding without floor", "err", err)
		cpuFloors = nil
	}
	memFloors, err := r.PrometheusClient.QueryMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		logger.V(1).Info("per-pod memory floor query failed; proceeding without floor", "err", err)
		memFloors = nil
	}

	medianReplicas, err := r.PrometheusClient.QueryReplicaCountMedian(ctx, ns, ownerKind, ownerName, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("replica count query: %w", err)
	}
	replicas := recommender.EffectiveReplicas(medianReplicas, autoInfo.MinReplicas)
	logger.V(1).Info("effective replica divisor",
		"medianReplicas", medianReplicas, "autoMinReplicas", autoInfo.MinReplicas, "effective", replicas)

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination
	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		var rec workload.ContainerRecommendation
		hasData := false

		if total, ok := cpuTotals[c.Name]; ok {
			perPod := recommender.PerPodFromTotal(total, replicas)
			perPod = recommender.ApplyFloor(perPod, cpuFloors[c.Name])
			rec.CPURequest = recommender.ComputeCPURequest(perPod, rsCfg.CPU.Requests)
			logger.V(1).Info("computed CPU recommendation",
				"container", c.Name, "totalCores", total, "replicas", replicas,
				"perPodCores", perPod, "request", quantityString(rec.CPURequest))
			hasData = true
		}

		// Memory: emit a recommendation when EITHER usage samples are present,
		// OR a recent OOM gives us a peak/current floor to anchor on.
		total, hasUsage := memTotals[c.Name]
		_, hasPeak := oomSignal.PeakMemoryBytes[c.Name]
		liveRec := liveOOMs[c.Name]
		hasLive := liveRec != nil
		if hasUsage || (recentOOM && hasPeak) || hasLive {
			var perPod float64
			if hasUsage {
				perPod = recommender.PerPodFromTotal(total, replicas)
				perPod = recommender.ApplyFloor(perPod, memFloors[c.Name])
			}
			oom := recommender.NewOOMSignal(recentOOM, oomSignal.PeakMemoryBytes[c.Name], oomSignal.OOMLimitBytes[c.Name])
			if hasLive {
				oom.LiveEventAt = liveRec.TerminatedAt
				// Fall back to the cache-captured limit when Prometheus has not
				// yet surfaced it. The cache value is the cgroup limit the
				// kernel killed at, captured from the pod spec at OOM time.
				if oom.OOMTimeLimitBytes == 0 && liveRec.OOMLimitBytes > 0 {
					oom.OOMTimeLimitBytes = float64(liveRec.OOMLimitBytes)
				}
			}
			if cur := c.Resources.Requests.Memory(); cur != nil && !cur.IsZero() {
				oom.CurrentRequestBytes = float64(cur.Value())
			}
			memQty, floorApplied := recommender.ComputeMemoryRequestWithOOMFloorReport(perPod, oom, rsCfg.Memory.Requests)
			rec.MemoryRequest = memQty
			if floorApplied {
				oomFloorApplied.WithLabelValues(ns, ownerKind, ownerName, c.Name).Inc()
				if hasLive && !liveRec.TerminatedAt.IsZero() {
					EmitOOMReactionLatency(ns, ownerKind, ownerName, time.Since(liveRec.TerminatedAt).Seconds())
				}
			}
			logger.V(1).Info("computed memory recommendation",
				"container", c.Name, "hasUsage", hasUsage, "totalBytes", total, "replicas", replicas,
				"perPodBytes", perPod, "oomRecent", oom.Recent, "oomPeak", oom.PeakBytes,
				"request", quantityString(rec.MemoryRequest))
			hasData = true
		}

		if !hasData {
			continue
		}

		// Apply autoscaler coordination (overhead + replica budget) before
		// limits are derived, so limits track the adjusted requests.
		base := rec
		rec = recommender.ApplyCoordination(rec, coordCfg, autoInfo, rsCfg)
		emitCoordinationFactors(ns, ownerKind, ownerName, coordCfg, autoInfo, base, rec)

		// Re-derive limits from the (possibly) adjusted requests.
		if rec.CPURequest != nil {
			lr := recommender.ComputeLimit(rec.CPURequest, c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu(), rsCfg.CPU.Limits)
			rec.CPULimit = lr.Quantity
			rec.RemoveCPULimit = lr.Remove
		}
		if rec.MemoryRequest != nil {
			lr := recommender.ComputeLimit(rec.MemoryRequest, c.Resources.Requests.Memory(), c.Resources.Limits.Memory(), rsCfg.Memory.Limits)
			rec.MemoryLimit = lr.Quantity
			rec.RemoveMemoryLimit = lr.Remove
		}

		recs[c.Name] = rec
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
