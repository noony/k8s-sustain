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

// apply applies every identity's result to its live members not in backoff,
// one task per member so a large owner-name group spreads across the slots,
// and returns exactly one result per live member, backed-off ones included.
// It keeps no retry or health state: settle records the results.
//
// An identity whose decision persist could not store is not applied: the
// replacements of the pods it would evict read the stored Recommendation at
// admission, and would start on template resources. Its members fail on the
// store step instead.
func (r *PolicyReconciler) apply(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	results []identityResult,
	autoSnap *autoscaler.NamespacedSnapshot,
) []applyResult {
	logger := log.FromContext(ctx)
	n := 0
	for i := range results {
		n += len(results[i].backedOff) + len(results[i].apply)
	}
	out := make([]applyResult, n)
	slot := 0
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range results {
		res := &results[i]
		for _, t := range res.backedOff {
			logger.V(1).Info("skipping workload in retry backoff", "target", t.key())
			out[slot] = applyResult{member: t, kind: applySkipped}
			slot++
		}
		if res.recordErr != nil && len(res.apply) > 0 {
			id := res.item.Identity
			logger.Info("not applying: the identity's recommendation could not be stored for the webhook",
				"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace,
				"members", len(res.apply), "err", res.recordErr.Error())
			for _, t := range res.apply {
				out[slot] = applyResult{member: t, kind: applyFailed, phase: phaseStore, err: res.recordErr}
				slot++
			}
			continue
		}
		for _, t := range res.apply {
			dst := &out[slot]
			slot++
			g.Go(func() error {
				*dst = r.reconcileWorkload(ctx, policy, t, autoSnap, res.recs, res.err)
				return nil
			})
		}
	}
	_ = g.Wait()
	return out
}

// settle records every member's result in the ledger and emits the events the
// ledger calls for. It returns how many members failed this pass and how many
// were skipped.
func (r *PolicyReconciler) settle(ctx context.Context, results []applyResult) (failed, skipped int) {
	logger := log.FromContext(ctx)
	for _, res := range results {
		if res.kind == applySkipped {
			skipped++
		}
		ev, isFailure := r.ledger.record(ctx, res)
		if !isFailure {
			continue
		}
		failed++
		t := res.member
		IncrementRetryAttempt(t.identity())
		logger.Error(res.err, "applying the recommendation failed",
			"kind", t.Kind, "name", t.Name, "namespace", t.Namespace, "phase", res.phase)
		if ev != nil {
			r.recorder.Eventf(t.Object, nil, corev1.EventTypeWarning, ev.reason, ev.reason, "%s", ev.message)
		}
	}
	return failed, skipped
}

// reconcileWorkload APPLIES an identity's recommendation to a single workload
// target: recycles or resizes its pods and emits its events. It does not
// compute anything and does not write the WorkloadRecommendation — the
// recommendation pass and persist did both, once for the identity, before any
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
