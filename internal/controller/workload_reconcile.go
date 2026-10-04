package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

// apply applies every identity's result to its live members not in backoff,
// one task per member so a large owner-name group spreads across the slots. It
// returns how many members it dispatched, how many of those failed, and how
// many it skipped for backoff.
func (r *PolicyReconciler) apply(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	results []identityResult,
	autoSnap *autoscaler.NamespacedSnapshot,
) (dispatched, failed, skipped int) {
	logger := log.FromContext(ctx)
	var failures atomic.Int32
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range results {
		res := &results[i]
		for _, t := range res.backedOff {
			logger.V(1).Info("skipping workload in retry backoff", "target", t.key())
			skipped++
		}
		for _, t := range res.apply {
			dispatched++
			g.Go(func() error {
				if err := r.reconcileWorkload(ctx, policy, t, autoSnap, res.recs, res.err); err != nil {
					failures.Add(1)
				}
				return nil
			})
		}
	}
	_ = g.Wait()
	return dispatched, int(failures.Load()), skipped
}

// reconcileWorkload APPLIES an identity's recommendation to a single workload
// target: recycles or resizes its pods, emits events, records its pod counts
// for the identity's health series, and tracks retries. It does not compute
// anything and does not write the WorkloadRecommendation — the recommendation
// pass and persist did both, once for the identity, before any member reached
// this function.
//
// recs is that shared recommendation, covering the union of the identity's
// members' containers; this function narrows it to the containers this member
// actually runs. computeErr is the identity's fetch failure, surfaced through
// handleStepError("prometheus", ...) so retry tracking, the
// ReconciliationRetryScheduled event and the PartialFailure condition stay
// keyed per real workload object, which is what retry state is keyed on.
func (r *PolicyReconciler) reconcileWorkload(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	t *workloadTarget,
	autoSnap *autoscaler.NamespacedSnapshot,
	recs map[string]workload.ContainerRecommendation,
	computeErr error,
) error {
	// Early bail-out if the parent reconcile context has already been cancelled
	// (typically a manager shutdown mid-batch). Without this, queued workers
	// still kick off autoscaler detection + Prometheus queries that will
	// each fail through their own timeout path, slowing graceful shutdown.
	if err := ctx.Err(); err != nil {
		return nil //nolint:nilerr // ctx-cancel is graceful shutdown, not a workload error
	}

	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	tol := buildTolerance(policy.Spec.RightSizing.ResourcesConfigs)
	suppressionObserver := func(resource string) {
		EmitRecycleSuppressed(t.identity(), resource)
	}
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
		return r.handleStepError(ctx, t, "prometheus", "Prometheus query failed", computeErr)
	}

	// The identity's recommendation covers the union of its members'
	// containers; this member applies only the subset it declares.
	recs = recsForTarget(recs, containers)
	if len(recs) == 0 {
		logger.V(1).Info("no recommendations available yet (no Prometheus data)")
		r.health.clearPods(t.key())
		r.recordStepSuccess(t)
		return nil
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
		r.health.clearPods(t.key())
		r.recordStepSuccess(t)
		return nil
	}

	var counts workload.PodCounts
	applyOpts := []workload.RecycleOption{workload.WithPodCounts(&counts)}
	// OnCreate never touches running pods; the dry run only measures how many
	// still wait for a rollout to pick up the recommendation.
	if t.UpdateMode == sustainv1alpha1.UpdateModeOnCreate {
		applyOpts = append(applyOpts, workload.WithDryRun())
	}

	// Bare-pod identities (Kind == "Pod") are NEVER evicted: no controller
	// would recreate the pod. In-place resize needs no controller, so on
	// clusters that support it the running pods are corrected directly —
	// otherwise a long-running Airflow task would stay on whatever it was
	// admitted with, forever. Below k8s 1.33 resizeInPlaceTarget is a no-op
	// and bare pods stay untouched.
	//
	// OnCreate bare pods reach this branch only as a dry run (WithDryRun in
	// applyOpts): they are counted, never resized.
	if t.Kind == "Pod" {
		return r.resizeInPlaceTarget(ctx, t, containers, recs, tol, &counts, func() (int, error) {
			return r.resizeBarePods(ctx, t, recs, tol, suppressionObserver, applyOpts...)
		})
	}

	// CronJob: never mutate the CronJob spec (would cause GitOps drift) and
	// never evict job pods (would kill in-flight runs). On clusters that
	// support InPlacePodVerticalScaling we resize the currently-running job
	// pods directly; new scheduled runs always pick up the latest resources
	// from the webhook at admission time.
	if t.Kind == "CronJob" {
		return r.resizeInPlaceTarget(ctx, t, containers, recs, tol, &counts, func() (int, error) {
			return r.resizeCronJobPods(ctx, t, recs, tol, suppressionObserver, applyOpts...)
		})
	}

	// Standalone Job: never mutate the Job spec and never evict job pods
	// (killing them discards in-flight work). A standalone Job has no next
	// run, so resizing the running pod in place is the only way to correct it
	// after creation. CronJob-owned Jobs never reach here — the listing path
	// excludes them and the CronJob branch above handles them.
	if t.Kind == "Job" {
		return r.resizeInPlaceTarget(ctx, t, containers, recs, tol, &counts, func() (int, error) {
			return r.resizeJobPods(ctx, t, recs, tol, suppressionObserver, applyOpts...)
		})
	}

	sel, err := metav1.LabelSelectorAsSelector(t.Selector)
	if err != nil {
		r.retries.clear(t.key())
		return err
	}

	// Identify the target so the patcher only touches pods owned by this
	// workload — a bare pod or an overlapping selector from a workload that
	// did not opt in must never be recycled.
	tw := workload.TargetWorkload{Kind: t.Kind, Name: t.Name}
	if t.Object != nil {
		tw.UID = t.Object.GetUID()
	}
	logger.V(1).Info("recycling pods", "selector", sel.String())
	recycled, err := r.patcher.RecyclePods(ctx, tw, t.Namespace, sel, recs,
		append([]workload.RecycleOption{
			workload.WithTolerance(tol),
			workload.WithSuppressionObserver(suppressionObserver),
			workload.WithIgnoreSafeToEvictAnnotations(policy.Spec.RightSizing.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations),
		}, applyOpts...)...,
	)
	if err != nil {
		if t.UpdateMode == sustainv1alpha1.UpdateModeOnCreate {
			return r.skipFailedDryRun(ctx, t, err)
		}
		return r.handleStepError(ctx, t, "patch", "Pod recycle failed", err)
	}

	r.health.setPods(t.key(), counts)
	r.recordStepSuccess(t)

	// The pod template is never patched, so it differs from the recommendation
	// on every reconcile: only the patcher's count says whether anything
	// happened. The container list stays a best-effort approximation — the
	// patcher decides per live pod, the template is all we have here.
	if recycled == 0 {
		logger.V(1).Info("no pod resized or evicted, no event emitted")
		return nil
	}
	changed := changedContainers(containers, recs, tol)
	if len(changed) == 0 {
		return nil
	}
	r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
		"Updated resources on %d pod(s) for containers: %v", recycled, changed)
	logger.Info("workload resources updated", "containers", changed, "pods", recycled)

	return nil
}

// resizeInPlaceTarget runs the reconcile tail shared by the CronJob and
// standalone Job paths. Both resize their currently-running pods in place and
// never evict them or mutate the workload spec, so the only kind-specific part
// is the pod enumeration + resize, supplied as resizeFn (it returns the number
// of pods the API server actually resized). The ResourcesUpdated event is only
// emitted when at least one pod was resized — the workload spec is never
// mutated, so changedContainers alone would fire on every reconcile.
func (r *PolicyReconciler) resizeInPlaceTarget(ctx context.Context, t *workloadTarget, containers []corev1.Container, recs map[string]workload.ContainerRecommendation, tol workload.Tolerance, counts *workload.PodCounts, resizeFn func() (int, error)) error {
	resized, err := resizeFn()
	if err != nil {
		if t.UpdateMode == sustainv1alpha1.UpdateModeOnCreate {
			return r.skipFailedDryRun(ctx, t, err)
		}
		return r.handleStepError(ctx, t, "resize", t.Kind+" pod resize failed", err)
	}
	r.recordStepSuccess(t)
	r.health.setPods(t.key(), *counts)
	if resized > 0 {
		if changed := changedContainers(containers, recs, tol); len(changed) > 0 {
			r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
				"In-place resized %d %s pod(s) for containers: %v", resized, strings.ToLower(t.Kind), changed)
		}
	}
	return nil
}

// skipFailedDryRun treats a failed OnCreate count-only pass as success: the
// pass never touches pods, and retry backoff would skip the compute phase and
// freeze the WorkloadRecommendation the webhook relies on. The member keeps
// its previous pod counts.
func (r *PolicyReconciler) skipFailedDryRun(ctx context.Context, t *workloadTarget, err error) error {
	log.FromContext(ctx).Info("OnCreate stale-pod count failed, pod counts left unchanged",
		"kind", t.Kind, "name", t.Name, "namespace", t.Namespace, "error", err.Error())
	r.recordStepSuccess(t)
	return nil
}

// handleStepError applies the retry/event/metric policy for a failed reconcile
// step. Returns nil for non-transient errors (drop from retry tracker, no
// requeue) or err for transient ones (record attempt and phase, schedule
// retry, emit event). phase becomes the identity's retry-state reason when
// this member is the one it reports; msg is the human-readable description
// used in the event and log line.
func (r *PolicyReconciler) handleStepError(ctx context.Context, t *workloadTarget, phase, msg string, err error) error {
	if !isTransientError(err) {
		r.retries.clear(t.key())
		// Context cancellation is graceful shutdown, not a misconfiguration —
		// don't spam events/logs on the way out.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Permanent errors (e.g. 403 from missing RBAC) are never retried,
			// so surface them loudly — otherwise they fail invisibly forever.
			r.recorder.Eventf(t.Object, nil, corev1.EventTypeWarning, "ReconciliationFailed", "ReconciliationFailed",
				"%s: %v. Permanent error, not retrying", msg, err)
			log.FromContext(ctx).Error(err, msg+", permanent error, not retrying", "phase", phase)
		}
		return nil
	}
	// Take the state recordFailure computed under its own lock rather than
	// reading it back: another goroutine holding the same workload key can
	// clear it in between (recordSuccess deletes the entry), and this runs
	// inside an errgroup closure, where a nil deref takes the process down.
	state := r.retries.recordFailure(t.key(), phase)
	r.recorder.Eventf(t.Object, nil, corev1.EventTypeWarning, "ReconciliationRetryScheduled", "ReconciliationRetryScheduled",
		"%s: %v. Retry attempt %d at %s", msg, err, state.attempts, state.nextRetry.Format(time.RFC3339))
	log.FromContext(ctx).Error(err, msg+", retry scheduled", "attempt", state.attempts)
	IncrementRetryAttempt(t.identity())
	return err
}

// recordStepSuccess clears retry state for a workload target whose latest
// step succeeded.
func (r *PolicyReconciler) recordStepSuccess(t *workloadTarget) {
	r.retries.clear(t.key())
}
