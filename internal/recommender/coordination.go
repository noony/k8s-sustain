package recommender

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

// Constants used by autoscaler-coordination math. The safety margin is the
// fraction (as a percentage) above the HPA threshold at which usage still
// sits after request shaping — i.e. 110 means usage stays ~10% below the
// target. The target clamp protects the formula from degenerate values
// (e.g. someone configures averageUtilization=0).
const (
	overheadSafetyMarginPct int32 = 110
	overheadTargetMin       int32 = 1
	overheadTargetMax       int32 = 99
)

// ApplyOverhead scales qty by (overheadSafetyMarginPct / target_pct).
// Returns qty unchanged (a deep copy) when target_pct <= 0 (no HPA target),
// or nil when qty is nil. target_pct is clamped to [1, 99] before division.
//
// Math is in millivalues for CPU precision; memory quantities use the same
// scaling and round up to the next milli-byte (the result is then turned
// back into a Quantity that callers can compare to MaxAllowed clamps).
//
// Exported so callers (the controller) can compute overhead-only ratios for
// observability without re-running the full ApplyCoordination pipeline.
func ApplyOverhead(qty *resource.Quantity, targetPct int32) *resource.Quantity {
	if qty == nil {
		return nil
	}
	if targetPct <= 0 {
		cp := qty.DeepCopy()
		return &cp
	}
	if targetPct < overheadTargetMin {
		targetPct = overheadTargetMin
	}
	if targetPct > overheadTargetMax {
		targetPct = overheadTargetMax
	}
	raw := float64(qty.MilliValue()) * float64(overheadSafetyMarginPct) / float64(targetPct)
	return resource.NewMilliQuantity(int64(math.Ceil(raw)), qty.Format)
}

const (
	replicaFactorMin = 0.5
	replicaFactorMax = 2.0
)

// applyReplicaCorrection nudges qty by clamp(current/target_replicas, 0.5, 2.0)
// where target_replicas = round(min + anchor * (max - min)). The factor
// pushes workloads above the budget anchor toward consolidation (factor > 1)
// and workloads below toward spreading (factor < 1).
//
// No-op (returns a deep copy of qty) when:
//   - qty is nil → nil
//   - anchor is nil
//   - max <= min (no replica budget)
//   - current <= 0 (workload scaled to zero)
//
// Anchor is clamped to [0, 1]; target_replicas is clamped to [min, max].
func applyReplicaCorrection(qty *resource.Quantity, anchor *float64, current, minR, maxR int32) *resource.Quantity {
	if qty == nil {
		return nil
	}
	cp := qty.DeepCopy()
	if anchor == nil || maxR <= minR || current <= 0 {
		return &cp
	}
	a := *anchor
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	target := int32(math.Round(float64(minR) + a*float64(maxR-minR)))
	if target < minR {
		target = minR
	}
	if target > maxR {
		target = maxR
	}
	if target <= 0 {
		return &cp
	}
	raw := float64(current) / float64(target)
	if raw < replicaFactorMin {
		raw = replicaFactorMin
	}
	if raw > replicaFactorMax {
		raw = replicaFactorMax
	}
	scaled := float64(cp.MilliValue()) * raw
	return resource.NewMilliQuantity(int64(math.Ceil(scaled)), qty.Format)
}

// ApplyCoordination layers the overhead formula and (optionally) the replica
// correction onto a baseline ContainerRecommendation. No-op when cfg.Enabled
// is false or when no autoscaler targets the workload.
//
// CPU receives both overhead and replica correction; memory receives only
// overhead because memory consumption doesn't track requests the way CPU
// does, so replica-budget bumping on memory wouldn't change HPA behaviour.
//
// MinAllowed/MaxAllowed clamps from res are re-applied to the adjusted
// requests so explicit operator caps survive coordination. Limits are NOT
// recomputed here — callers derive limits from the adjusted requests using
// the existing ComputeLimit logic.
func ApplyCoordination(
	base workload.ContainerRecommendation,
	cfg sustainv1alpha1.AutoscalerCoordination,
	info autoscaler.Info,
	res sustainv1alpha1.ResourcesConfigs,
) workload.ContainerRecommendation {
	if !cfg.Enabled || info.Kind == autoscaler.KindNone {
		return base
	}

	out := base

	if base.CPURequest != nil {
		adjusted := ApplyOverhead(base.CPURequest, info.ConfiguredTargets[autoscaler.ResourceCPU])
		adjusted = applyReplicaCorrection(adjusted, cfg.ReplicaBudgetAnchor, info.CurrentReplicas, info.MinReplicas, info.MaxReplicas)
		clampQuantity(adjusted, res.CPU.Requests.MinAllowed, res.CPU.Requests.MaxAllowed)
		out.CPURequest = adjusted
	}

	if base.MemoryRequest != nil {
		adjusted := ApplyOverhead(base.MemoryRequest, info.ConfiguredTargets[autoscaler.ResourceMemory])
		clampQuantity(adjusted, res.Memory.Requests.MinAllowed, res.Memory.Requests.MaxAllowed)
		out.MemoryRequest = adjusted
	}

	return out
}
