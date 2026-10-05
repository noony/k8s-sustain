package wlrcache

import (
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Standing is where an identity stands in the snapshot a sweep judges its
// object against.
type Standing int

const (
	// Ungoverned: the identity has live members no Policy governs, or the
	// snapshot does not hold it (its namespace or kind left the scope).
	Ungoverned Standing = iota
	// Governed: a Policy governs the identity, the sweeping one or another
	// that adopts the object on its next reconcile.
	Governed
	// Departed: the identity has no live member left.
	Departed
	// Conflicted: the identity's members are governed by different Policies.
	Conflicted
)

// sweepGrace protects an object created after the snapshot a sweep judges it
// against. Anchored on creation, not ObservedAt, which every pass rewrites and
// would make the guard self-satisfying.
const sweepGrace = 10 * time.Minute

// Expired reports whether policy's sweep deletes known, whose identity stands
// at s. An object naming another Policy is that Policy's to judge, and one
// created within the grace period is kept. A Departed or Conflicted
// identity's Recommendation is kept for the retention window (zero or less
// keeps nothing), counted from when it was last computed; an Ungoverned
// identity's object is deleted.
func Expired(known *sustainv1alpha1.WorkloadRecommendation, policy string, s Standing, now time.Time, retention time.Duration) bool {
	if known.Spec.Policy != policy {
		return false
	}
	if now.Sub(known.CreationTimestamp.Time) < sweepGrace {
		return false
	}
	switch s {
	case Ungoverned:
		return true
	case Departed, Conflicted:
		return retention <= 0 || now.Sub(lastComputed(known)) > retention
	default:
		return false
	}
}

// lastComputed is ObservedAt, or the creation time when nothing was computed
// since.
func lastComputed(known *sustainv1alpha1.WorkloadRecommendation) time.Time {
	if known.CreationTimestamp.After(known.Status.ObservedAt.Time) {
		return known.CreationTimestamp.Time
	}
	return known.Status.ObservedAt.Time
}
