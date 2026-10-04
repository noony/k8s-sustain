package recommender

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
)

// Autoscaler-coordination tuning. Scaling a request by
// safetyMargin / target_pct settles steady-state utilisation at
// target_pct / safetyMargin — with 110 and a target of 80, ≈72.7%, some 7
// points under the HPA threshold, enough to absorb short spikes without
// triggering scale-up. The target clamp guards against degenerate configs
// (averageUtilization of 0 or 100).
const (
	overheadSafetyMarginPct int32 = 110
	overheadTargetMin       int32 = 1
	overheadTargetMax       int32 = 99
)

// applyOverhead scales qty by (overheadSafetyMarginPct / target_pct) and
// returns the factor it applied. qty is returned unchanged, with factor 1,
// when target_pct <= 0 (no autoscaler target on this resource). target_pct is
// clamped to [1, 99] before division.
//
// Math is in millivalues for CPU precision; memory quantities (BinarySI)
// are scaled in whole bytes and rounded up to the next byte — milli math
// would produce fractional-byte quantities whenever 110/target isn't
// byte-exact, and Kubernetes warns on fractional byte values in pod specs.
func applyOverhead(qty resource.Quantity, targetPct int32) (resource.Quantity, float64) {
	if targetPct <= 0 {
		return qty, 1
	}
	targetPct = min(max(targetPct, overheadTargetMin), overheadTargetMax)
	factor := float64(overheadSafetyMarginPct) / float64(targetPct)
	// Multiply before dividing, not by factor: 110/target is rarely exact in
	// float64, and the ceil would turn its error into a whole unit.
	if qty.Format == resource.BinarySI {
		raw := float64(qty.Value()) * float64(overheadSafetyMarginPct) / float64(targetPct)
		return *resource.NewQuantity(int64(math.Ceil(raw)), qty.Format), factor
	}
	raw := float64(qty.MilliValue()) * float64(overheadSafetyMarginPct) / float64(targetPct)
	return *resource.NewMilliQuantity(int64(math.Ceil(raw)), qty.Format), factor
}

const (
	replicaFactorMin = 0.5
	replicaFactorMax = 2.0
)

// applyReplicaCorrection nudges qty by clamp(current/target_replicas, 0.5, 2.0)
// where target_replicas = round(min + anchor * (max - min)), and returns the
// factor it applied. The factor pushes workloads above the budget anchor
// toward consolidation (factor > 1) and workloads below toward spreading
// (factor < 1).
//
// No-op (qty unchanged, factor 1) when max <= min (no replica budget) or
// current <= 0 (workload scaled to zero).
//
// Anchor is clamped to [0, 1]; target_replicas is clamped to [min, max].
func applyReplicaCorrection(qty resource.Quantity, anchor float64, current, minR, maxR int32) (resource.Quantity, float64) {
	if maxR <= minR || current <= 0 {
		return qty, 1
	}
	a := min(max(anchor, 0), 1)
	target := int32(math.Round(float64(minR) + a*float64(maxR-minR)))
	target = min(max(target, minR), maxR)
	if target <= 0 {
		return qty, 1
	}
	factor := min(max(float64(current)/float64(target), replicaFactorMin), replicaFactorMax)
	scaled := float64(qty.MilliValue()) * factor
	return *resource.NewMilliQuantity(int64(math.Ceil(scaled)), qty.Format), factor
}

// coordinate shapes a clamped request for the autoscaler targeting the
// workload: the overhead for the autoscaler's utilization target on res, then
// for CPU the replica correction when the Policy sets a ReplicaBudgetAnchor,
// then the min/max clamp again so explicit operator caps survive
// coordination. Nil when coordination is off or no autoscaler targets the
// workload.
//
// Memory receives only the overhead because memory consumption doesn't track
// requests the way CPU does, so replica-budget bumping on memory wouldn't
// change HPA behaviour.
func coordinate(
	clamped resource.Quantity,
	res resourceKind,
	cfg sustainv1alpha1.AutoscalerCoordination,
	info autoscaler.Info,
	req sustainv1alpha1.ResourceRequestsConfig,
) *sustainv1alpha1.CoordinationTrace {
	if !cfg.Enabled || info.Kind == autoscaler.KindNone {
		return nil
	}
	scaled, overhead := applyOverhead(clamped, info.ConfiguredTargets[string(res)])
	t := &sustainv1alpha1.CoordinationTrace{OverheadFactor: overhead}
	if res == cpuResource && cfg.ReplicaBudgetAnchor != nil {
		var replica float64
		scaled, replica = applyReplicaCorrection(scaled, *cfg.ReplicaBudgetAnchor, info.CurrentReplicas, info.MinReplicas, info.MaxReplicas)
		t.ReplicaFactor = &replica
	}
	t.Scaled = scaled
	t.Value = clamp(scaled, req.MinAllowed, req.MaxAllowed)
	return t
}
