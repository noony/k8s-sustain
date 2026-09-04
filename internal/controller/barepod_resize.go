package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/workload"
)

// resizeBarePods corrects the running pods of a bare-pod identity in place.
//
// Bare pods are NEVER evicted — no controller would recreate them — but an
// in-place resize needs no controller behind it. It can still restart a
// container and discard an Airflow task's in-flight work; that is the same
// tradeoff Job and CronJob already make, bounded by downsize suppression.
//
// Membership comes from workload.GroupBarePods rather than a label selector:
// the mirrored owner-name label only exists on pods the webhook admitted, and
// the grouping helper additionally excludes controller-owned pods and pods
// naming a different policy. Those two are the bare-pod analogue of the
// ownerRef-UID and selector checks protecting every other kind.
//
// The grouping runs ONCE per (policy, namespace), in listBarePodTargets, and
// the members ride along on the target — see workloadTarget.BarePodMembers.
//
// Returns the number of pods the API server actually resized, so
// resizeInPlaceTarget can suppress the ResourcesUpdated event when nothing was
// touched.
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
