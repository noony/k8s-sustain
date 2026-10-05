package controller

import (
	"context"
	"strings"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

// apply applies every identity's verdict to its live members not in backoff,
// one task per member so a large owner-name group spreads across the slots,
// and returns, per identity, exactly one result per live member, backed-off
// ones included. It keeps no retry or health state: the ledger records the
// results from the pass's report.
//
// An identity whose decision could not be stored is not applied: the
// replacements of the pods it would evict read the stored Recommendation at
// admission, and would start on template resources. Its members fail on the
// store step instead.
func (r *PolicyReconciler) apply(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	work []passIdentity,
	recs []recommendation,
	stored []error,
	autoSnap *autoscaler.NamespacedSnapshot,
) [][]applyResult {
	logger := log.FromContext(ctx)
	out := make([][]applyResult, len(work))
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i, w := range work {
		out[i] = make([]applyResult, len(w.backedOff)+len(w.run))
		for j, t := range w.backedOff {
			logger.V(1).Info("skipping workload in retry backoff", "target", t.key())
			out[i][j] = applyResult{member: t, kind: applySkipped}
		}
		run := out[i][len(w.backedOff):]
		if stored[i] != nil && len(w.run) > 0 {
			id := w.id.Key
			logger.Info("not applying: the identity's recommendation could not be stored for the webhook",
				"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace,
				"members", len(w.run), "err", stored[i].Error())
			for j, t := range w.run {
				run[j] = applyResult{member: t, kind: applyFailed, phase: phaseStore, err: stored[i]}
			}
			continue
		}
		for j, t := range w.run {
			g.Go(func() error {
				run[j] = r.reconcileWorkload(ctx, policy, t, autoSnap, recs[i].recs, recs[i].err)
				return nil
			})
		}
	}
	_ = g.Wait()
	return out
}

// reconcileWorkload APPLIES an identity's recommendation to a single workload
// target: recycles or resizes its pods and emits its events. It does not
// compute anything and does not write the WorkloadRecommendation — the
// recommendation pass and its record did both, once for the identity, before any
// member reached this function.
//
// recs is that shared recommendation, covering the union of the identity's
// members' containers; this function narrows it to the containers this member
// actually runs. computeErr is the identity's fetch failure, reported as each
// member's failed prometheus step, because retry state is kept per real
// workload object.
func (r *PolicyReconciler) reconcileWorkload(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	t *workloadTarget,
	autoSnap *autoscaler.NamespacedSnapshot,
	recs map[string]workload.ContainerRecommendation,
	computeErr error,
) applyResult {
	// Early bail-out if the parent reconcile context has already been cancelled
	// (typically a manager shutdown mid-batch). Without this, queued workers
	// still kick off autoscaler detection + Prometheus queries that will
	// each fail through their own timeout path, slowing graceful shutdown.
	if ctx.Err() != nil {
		return applyResult{member: t, kind: applySkipped}
	}

	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	tol := buildTolerance(policy.Spec.RightSizing.ResourcesConfigs)
	containers, initNames := t.recommendableContainers(excludeInit)
	logger.V(1).Info("reconciling workload",
		"containers", len(t.Containers),
		"initContainers", len(t.InitContainers),
		"excludeInitContainers", excludeInit,
		"recommending", len(containers))

	// Only for the member's event: the identity's autoscaler series follow the
	// autoscaler computeIdentity shaped the recommendation with.
	autoInfo, autoErr := autoSnap.Lookup(ctx, t.Namespace, t.Kind, t.Name)
	if autoErr != nil {
		logger.Error(autoErr, "autoscaler detection failed, proceeding without it")
		autoInfo = autoscaler.Info{Kind: autoscaler.KindNone}
	}
	if autoInfo.Kind != autoscaler.KindNone {
		r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "AutoscalerDetected", "AutoscalerDetected",
			"%s %s detected targeting %s/%s (replicas %d–%d)",
			autoInfo.Kind, autoInfo.Name, t.Kind, t.Name, autoInfo.MinReplicas, autoInfo.MaxReplicas)
	}

	if computeErr != nil {
		return applyResult{member: t, kind: applyFailed, phase: phasePrometheus, err: computeErr}
	}

	// The identity's recommendation covers the union of its members'
	// containers; this member applies only the subset it declares.
	recs = workload.Inject(t.Containers, t.InitContainers, excludeInit, recs).Recs
	if len(recs) == 0 {
		logger.V(1).Info("no recommendations available yet (no Prometheus data)")
		return applyResult{member: t, kind: applyIdle}
	}

	logger.Info("computed recommendations", "containers", len(recs))
	logger.V(1).Info("recommendation details", "recommendations", recs)

	emitWorkloadFromRecs(t, policy.Name, recs, initNames)

	if policy.EffectiveRecommendOnly(r.RecommendOnly) {
		source := "policy"
		if r.RecommendOnly {
			source = "flag"
		}
		logger.Info("recommend-only: computed recommendations", "source", source, "recommendations", recs)
		return applyResult{member: t, kind: applyIdle}
	}

	out, err := r.patcher.Apply(ctx, t.member(), recs, workload.ApplySettings{
		// OnCreate never touches running pods; the dry run only measures how
		// many still wait for a rollout to pick up the recommendation.
		DryRun:            t.UpdateMode == sustainv1alpha1.UpdateModeOnCreate,
		Tolerance:         tol,
		IgnoreSafeToEvict: policy.Spec.RightSizing.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations,
	})
	for resource, n := range out.Suppressed {
		EmitRecycleSuppressed(t.identity(), resource, n)
	}
	if err != nil {
		switch {
		case t.UpdateMode == sustainv1alpha1.UpdateModeOnCreate:
			logger.Info("OnCreate stale-pod count failed, pod counts left unchanged", "error", err.Error())
			return applyResult{member: t, kind: applyUncounted}
		case out.InPlaceOnly:
			return applyResult{member: t, kind: applyFailed, phase: phaseResize, err: err}
		default:
			return applyResult{member: t, kind: applyFailed, phase: phasePatch, err: err}
		}
	}
	done := applyResult{member: t, kind: applyDone, pods: out.Pods}

	// The pod template is never patched, so it differs from the recommendation
	// on every reconcile: only the patcher's count says whether anything
	// happened. The container list stays a best-effort approximation — the
	// patcher decides per live pod, the template is all we have here.
	if out.Changed == 0 {
		logger.V(1).Info("no pod resized or evicted, no event emitted")
		return done
	}
	changed := changedContainers(containers, recs, tol)
	if len(changed) == 0 {
		return done
	}
	if out.InPlaceOnly {
		r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
			"In-place resized %d %s pod(s) for containers: %v", out.Changed, strings.ToLower(t.Kind), changed)
	} else {
		r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
			"Updated resources on %d pod(s) for containers: %v", out.Changed, changed)
	}
	logger.Info("workload resources updated", "containers", changed, "pods", out.Changed)

	return done
}
