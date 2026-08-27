package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/workload"
)

// resizeBarePods corrects the running pods of a bare-pod identity in place.
//
// Bare pods are NEVER evicted — no controller would recreate them, which is
// why the reconcile path used to stop them before every apply path. That
// reasoning holds for eviction and not for in-place resize, which needs no
// controller behind it: the exception was written for one and over-applied to
// both, leaving long-running Airflow tasks stuck on whatever they were
// admitted with. An in-place memory resize can restart a container, and for an
// Airflow task that discards in-flight work — but this is the same tradeoff
// Job and CronJob already make, on the same reasoning (resizing the running
// pod is the only way to correct it after creation), so bare pods inherit it
// rather than inventing an exception. Downsize suppression below
// downsizeThreshold bounds how often it can fire.
//
// Membership comes from workload.GroupBarePods rather than a label selector.
// The mirrored owner-name label exists only on pods the webhook actually
// admitted, so pods created during a webhook outage would be missed — and more
// importantly the grouping helper skips any pod carrying a controller
// ownerRef, so a ReplicaSet-owned pod that happens to share the label is never
// mistaken for a member. It also excludes any pod naming a DIFFERENT policy
// from the group's, so this function can never resize a pod under a
// recommendation computed for someone else's policy. Together those two are
// the bare-pod analogue of the ownerRef-UID and selector checks protecting
// every other kind.
//
// The grouping runs ONCE per (policy, namespace), in listBarePodTargets, and
// the members ride along on the target. This function used to List every pod
// in the namespace and regroup them itself, per identity, per cycle,
// concurrently under the errgroup — M cache-backed Lists each deep-copying
// every pod in a namespace that for Airflow holds hundreds of them, to
// recompute a grouping the listing had already done. See
// workloadTarget.BarePodMembers.
//
// Returns the number of pods whose resize the API server actually accepted, so
// resizeInPlaceTarget can suppress the ResourcesUpdated event when nothing was
// touched (no live pods, in-place unsupported, or every resize skipped).
func (r *PolicyReconciler) resizeBarePods(
	ctx context.Context,
	t *workloadTarget,
	recs map[string]workload.ContainerRecommendation,
	tol workload.Tolerance,
	observe func(resource string),
) (int, error) {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.IdentityName, "namespace", t.Namespace)

	if len(t.BarePodMembers) == 0 {
		logger.V(1).Info("no live pods for bare-pod identity; nothing to resize")
		return 0, nil
	}

	logger.V(1).Info("resizing bare pods", "pods", len(t.BarePodMembers))
	return r.patcher.ResizePodsInPlace(ctx, t.BarePodMembers, recs,
		workload.WithTolerance(tol), workload.WithSuppressionObserver(observe))
}
