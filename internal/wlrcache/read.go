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
	// Withheld: the stored numbers belong to a Policy other than the pod's.
	Withheld
	// Stale: the Recommendation is older than the staleness budget or, when
	// it is retained, than the retention window.
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

// Freshness bounds how old a Recommendation may be and still be served. A
// zero field means its default.
type Freshness struct {
	// Staleness bounds a Recommendation the controller keeps refreshing.
	Staleness time.Duration
	// Retention bounds one it keeps frozen, for a Departed or Conflicted
	// identity. It must be the controller's --recommendation-retention.
	Retention time.Duration
}

func (f Freshness) staleness() time.Duration {
	if f.Staleness <= 0 {
		return DefaultStaleness
	}
	return f.Staleness
}

func (f Freshness) retention() time.Duration {
	if f.Retention <= 0 {
		return DefaultRetention
	}
	return f.Retention
}

// Reading is the verdict on one stored WorkloadRecommendation.
type Reading struct {
	Verdict Verdict
	// Recs is the Recommendation to inject, set only when the verdict injects.
	Recs map[string]workload.ContainerRecommendation
}

// Read decides what known, the identity's stored WorkloadRecommendation (nil
// when it has none), means at now to a pod opting into policy.
func Read(known *sustainv1alpha1.WorkloadRecommendation, policy string, now time.Time, f Freshness) Reading {
	if known == nil {
		return Reading{Verdict: Absent}
	}
	if known.Spec.Policy != policy {
		return Reading{Verdict: Withheld}
	}
	// Checked before staleness: an object without a Recommendation has no
	// ObservedAt to age.
	if len(known.Status.Containers) == 0 {
		if known.Status.Outcome == "" {
			return Reading{Verdict: Undecided}
		}
		return Reading{Verdict: NoData}
	}
	// A retained Recommendation is exempt from the staleness budget: its
	// ObservedAt is deliberately frozen at the last Computed pass, so gating on
	// it would put a daily Job back on template resources on every run but its
	// first. The waiver is bounded here rather than left to the controller's
	// sweep, because the sweep is skipped whenever the inventory cannot be read,
	// and a wedged controller would otherwise disable the staleness gate
	// outright for these objects.
	retained := known.Status.Departed || known.Status.Outcome == sustainv1alpha1.OutcomeConflicted
	age := now.Sub(known.Status.ObservedAt.Time)
	switch {
	case retained && age > f.retention():
		return Reading{Verdict: Stale}
	case retained:
		return Reading{Verdict: Retained, Recs: recsFromStatus(known.Status)}
	case age > f.staleness():
		return Reading{Verdict: Stale}
	default:
		return Reading{Verdict: Fresh, Recs: recsFromStatus(known.Status)}
	}
}

func recsFromStatus(status sustainv1alpha1.WorkloadRecommendationStatus) map[string]workload.ContainerRecommendation {
	out := make(map[string]workload.ContainerRecommendation, len(status.Containers))
	for name, c := range status.Containers {
		out[name] = workload.ContainerRecommendation(c)
	}
	return out
}
