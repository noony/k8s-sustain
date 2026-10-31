package wlrcache

import (
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// Verdict is what a stored WorkloadRecommendation means to a pod being
// admitted: whether the pod gets its Recommendation, and if not, why.
type Verdict int

const (
	// Absent: no object exists for the identity. The only verdict that asks
	// for one to be created.
	Absent Verdict = iota
	// Undecided: the object exists but no pass has decided anything for it
	// yet. It is already in the controller's work-list.
	Undecided
	// NoData: a pass recorded an outcome, but no Recommendation exists yet.
	NoData
	// Withheld: the object, or the Recommendation it holds, belongs to a
	// Policy other than the pod's.
	Withheld
	// Stale: the Recommendation is past the servableUntil its controller
	// stamped, or carries none.
	Stale
	// Fresh: the Recommendation is current and is injected.
	Fresh
	// Retained: the Recommendation is kept frozen for a Departed or
	// Conflicted identity and is injected.
	Retained
)

// Injects reports whether the pod gets the Recommendation.
func (v Verdict) Injects() bool { return v == Fresh || v == Retained }

func (v Verdict) String() string {
	switch v {
	case Absent:
		return "absent"
	case Undecided:
		return "undecided"
	case NoData:
		return "nodata"
	case Withheld:
		return "withheld"
	case Stale:
		return "stale"
	case Fresh:
		return "fresh"
	case Retained:
		return "retained"
	default:
		return "unknown"
	}
}

// Reading is the verdict on one stored WorkloadRecommendation.
type Reading struct {
	Verdict Verdict
	// Recs is the Recommendation to inject, set only when the verdict injects.
	Recs map[string]workload.ContainerRecommendation
}

// Read decides what known, the identity's stored WorkloadRecommendation (nil
// when it has none), means at now to a pod opting into policy. A
// Recommendation is served only to pods of the Policy that computed it
// (ADR 0003): one the object holds from before another Policy adopted it is
// withheld until that Policy records its own, whatever it records meanwhile.
// It is served only before the servableUntil the controller stamped
// (ADR 0008): an absolute time, so a controller that stops writing stops
// injection.
func Read(known *sustainv1alpha1.WorkloadRecommendation, policy string, now time.Time) Reading {
	if known == nil {
		return Reading{Verdict: Absent}
	}
	if known.Spec.Policy != policy {
		return Reading{Verdict: Withheld}
	}
	// Checked before staleness: an object without a Recommendation has no
	// servableUntil.
	if len(known.Status.Containers) == 0 {
		if known.Status.Outcome == "" {
			return Reading{Verdict: Undecided}
		}
		return Reading{Verdict: NoData}
	}
	if known.Status.ComputedBy != policy {
		return Reading{Verdict: Withheld}
	}
	switch {
	case !now.Before(known.Status.ServableUntil.Time):
		return Reading{Verdict: Stale}
	case known.Status.Departed || known.Status.Outcome == sustainv1alpha1.OutcomeConflicted:
		return Reading{Verdict: Retained, Recs: recsFromStatus(known.Status)}
	default:
		return Reading{Verdict: Fresh, Recs: recsFromStatus(known.Status)}
	}
}

// Held is the Recommendation known holds that policy computed, however old:
// what the controller publishes for policy's members whatever this pass
// decided. Nil when known holds none of policy's.
func Held(known *sustainv1alpha1.WorkloadRecommendation, policy string) map[string]workload.ContainerRecommendation {
	if known == nil || known.Status.ComputedBy != policy || len(known.Status.Containers) == 0 {
		return nil
	}
	return recsFromStatus(known.Status)
}

func recsFromStatus(status sustainv1alpha1.WorkloadRecommendationStatus) map[string]workload.ContainerRecommendation {
	out := make(map[string]workload.ContainerRecommendation, len(status.Containers))
	for name, c := range status.Containers {
		out[name] = workload.ContainerRecommendation(c)
	}
	return out
}
