package wlrcache

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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

// Standings answers where each identity stands in the snapshot a sweep judges
// against.
type Standings func(promclient.WorkloadIdentity) Standing

// sweepGrace protects an object the snapshot may not account for: one created
// after the snapshot was taken, or just before, while the cache it was read
// from had not seen the identity's members yet. Anchored on the snapshot's
// instant, not the sweep's: a pass that waits on evictions ends long after it
// took its snapshot. And on creation, not ObservedAt, which every pass
// rewrites and would make the guard self-satisfying.
const sweepGrace = 10 * time.Minute

// Expired reports whether policy's sweep deletes known, whose identity stands
// at s in a snapshot taken at at. An object naming another Policy is that
// Policy's to judge, and one created within the grace period before the
// snapshot, or after it, is kept. A Departed or Conflicted identity's object
// is kept for the retention window past its last computation, the window its
// Recommendation is stamped servable for; an Ungoverned identity's object is
// deleted.
func Expired(known *sustainv1alpha1.WorkloadRecommendation, policy string, s Standing, at time.Time, l Lifetimes) bool {
	if known.Spec.Policy != policy {
		return false
	}
	if at.Sub(known.CreationTimestamp.Time) < sweepGrace {
		return false
	}
	switch s {
	case Ungoverned:
		return true
	case Departed, Conflicted:
		return at.Sub(lastComputed(known.CreationTimestamp, known.Status.ObservedAt).Time) > l.Retention
	default:
		return false
	}
}

// Sweep deletes policy's objects Expired judges expired: each judged by where
// standing says its identity stands in a snapshot taken at at. It returns how
// many it deleted, and the first failure to list or delete; an object
// rewritten since it was listed is left to the next sweep, which judges it
// afresh.
func Sweep(ctx context.Context, c client.Client, policy string, standing Standings, at time.Time, l Lifetimes) (int, error) {
	return deleteWhere(ctx, c, judgeNextTime, []client.ListOption{client.MatchingLabels{sustainv1alpha1.WLRPolicyLabel: policy}},
		func(known *sustainv1alpha1.WorkloadRecommendation) bool {
			return Expired(known, policy, standing(identityOf(known.Spec.WorkloadRef)), at, l)
		})
}

// Release deletes the objects of policy, which is being deleted: only those
// still naming it when they are deleted, since another Policy may have adopted
// one since it was listed (a GitOps rename creates the new Policy before
// deleting the old), and the adopted object is the new Policy's to keep. Any
// failure is returned, a conflict included, so the caller keeps policy's
// finalizer until a retry has listed and judged afresh.
func Release(ctx context.Context, c client.Client, policy string) (int, error) {
	return deleteWhere(ctx, c, failRelease, []client.ListOption{client.MatchingLabels{sustainv1alpha1.WLRPolicyLabel: policy}},
		func(known *sustainv1alpha1.WorkloadRecommendation) bool {
			return known.Spec.Policy == policy
		})
}

// Reap deletes every object whose spec.policy names a Policy that does not
// exist, catching force deletes and crashes mid-release. It returns how many
// it deleted, and the first failure to list or delete; an object rewritten
// since it was listed, such as one just adopted, is left to the next reap.
func Reap(ctx context.Context, c client.Client) (int, error) {
	var policies sustainv1alpha1.PolicyList
	if err := c.List(ctx, &policies); err != nil {
		return 0, fmt.Errorf("listing Policies: %w", err)
	}
	exists := make(map[string]bool, len(policies.Items))
	for i := range policies.Items {
		exists[policies.Items[i].Name] = true
	}
	return deleteWhere(ctx, c, judgeNextTime, nil, func(known *sustainv1alpha1.WorkloadRecommendation) bool {
		// One naming no Policy was written by none of ours; leave it to its writer.
		return known.Spec.Policy != "" && !exists[known.Spec.Policy]
	})
}

// onConflict is what a conflicting delete means to a deletion path.
type onConflict int

const (
	// judgeNextTime: the path judges the object again on its next run.
	judgeNextTime onConflict = iota
	// failRelease: the conflict is a failure, so the release is retried
	// instead of reported finished.
	failRelease
)

// deleteWhere deletes the objects listed with opts that expired judges
// expired. Every delete is conditioned on the UID and resourceVersion judged
// (ADR 0003): each path judges a copy from an informer cache, and an object
// rewritten since may have been adopted by another Policy, which a conflict
// hands back to a fresh judgement instead of deleting it. NotFound counts as
// deleted.
func deleteWhere(
	ctx context.Context,
	c client.Client,
	conflict onConflict,
	opts []client.ListOption,
	expired func(*sustainv1alpha1.WorkloadRecommendation) bool,
) (int, error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := c.List(ctx, &list, opts...); err != nil {
		return 0, fmt.Errorf("listing WorkloadRecommendations: %w", err)
	}
	logger := log.FromContext(ctx)
	deleted := 0
	var failed error
	for i := range list.Items {
		known := &list.Items[i]
		if !expired(known) {
			continue
		}
		err := c.Delete(ctx, known, client.Preconditions{UID: &known.UID, ResourceVersion: &known.ResourceVersion})
		switch {
		case err == nil || apierrors.IsNotFound(err):
			deleted++
		case apierrors.IsConflict(err) && conflict == judgeNextTime:
			logger.V(1).Info("WorkloadRecommendation changed since it was listed; leaving it to the next pass",
				"name", known.Name, "namespace", known.Namespace, "policy", known.Spec.Policy)
		default:
			logger.V(1).Info("failed to delete WorkloadRecommendation",
				"name", known.Name, "namespace", known.Namespace, "policy", known.Spec.Policy, "err", err)
			if failed == nil {
				failed = fmt.Errorf("deleting WorkloadRecommendation %s/%s: %w", known.Namespace, known.Name, err)
			}
		}
	}
	return deleted, failed
}

func identityOf(ref sustainv1alpha1.WorkloadReference) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: ref.Namespace, OwnerKind: ref.Kind, OwnerName: ref.Name}
}
