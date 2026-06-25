package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

// reconcileWorkload processes a single workload target: queries Prometheus,
// computes recommendations, recycles pods, emits events, and tracks retries.
func (r *PolicyReconciler) reconcileWorkload(ctx context.Context, policy *sustainv1alpha1.Policy, t *workloadTarget, autoSnap *autoscaler.NamespacedSnapshot) error {
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
		EmitRecycleSuppressed(t.Namespace, t.Kind, t.Name, resource)
	}
	containers, initNames := t.recommendableContainers(excludeInit)
	logger.V(1).Info("reconciling workload",
		"containers", len(t.Containers),
		"initContainers", len(t.InitContainers),
		"excludeInitContainers", excludeInit,
		"recommending", len(containers))

	// Detect HPA / KEDA ScaledObject (read-only). Used as a replica-count
	// fallback for workload-level recommendations and for observability.
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
	EmitAutoscalerPresent(t.Namespace, t.Kind, t.Name, string(autoInfo.Kind))
	EmitAutoscalerTargetsConfigured(t.Namespace, t.Kind, t.Name, string(autoInfo.Kind), autoInfo.ConfiguredTargets)

	workloadCreated := time.Time{}
	if t.Object != nil {
		workloadCreated = t.Object.GetCreationTimestamp().Time
	}
	recs, err := r.buildRecommendations(ctx, policy, t.Namespace, t.Kind, t.Name, containers, autoInfo, workloadCreated)
	if err != nil {
		return r.handleStepError(ctx, t, "prometheus", "Prometheus query failed", err)
	}

	if len(recs) == 0 {
		logger.V(1).Info("no recommendations available yet (no Prometheus data)")
		r.recordStepSuccess(t)
		return nil
	}

	logger.Info("computed recommendations", "containers", len(recs))
	logger.V(1).Info("recommendation details", "recommendations", recs)

	// Emit per-container recommendation/drift metrics before recycling pods.
	emitWorkloadFromRecs(t, policy.Name, recs, initNames)

	// Persist last-known-good recommendation as a WorkloadRecommendation.
	// Lets the webhook serve cached values during a Prometheus outage and
	// gives operators a `kubectl get wlrec` audit surface. Best-effort: the
	// upsert never propagates errors so a failed cache write can't block the
	// recycle path.
	r.upsertWorkloadRecommendation(ctx, t, policy.Name, recs, metav1.Now())

	if r.RecommendOnly {
		logger.Info("recommend-only: computed recommendations", "recommendations", recs)
		r.recordStepSuccess(t)
		return nil
	}

	// CronJob: never mutate the CronJob spec (would cause GitOps drift) and
	// never evict job pods (would kill in-flight runs). On clusters that
	// support InPlacePodVerticalScaling we resize the currently-running job
	// pods directly; new scheduled runs always pick up the latest resources
	// from the webhook at admission time.
	if t.Kind == "CronJob" {
		return r.resizeInPlaceTarget(ctx, t, containers, recs, tol, func() (int, error) {
			return r.resizeCronJobPods(ctx, t, recs, tol, suppressionObserver)
		})
	}

	// Standalone Job: never mutate the Job spec and never evict job pods
	// (killing them discards in-flight work). A standalone Job has no next
	// run, so resizing the running pod in place is the only way to correct it
	// after creation. CronJob-owned Jobs never reach here — the listing path
	// excludes them and the CronJob branch above handles them.
	if t.Kind == "Job" {
		return r.resizeInPlaceTarget(ctx, t, containers, recs, tol, func() (int, error) {
			return r.resizeJobPods(ctx, t, recs, tol, suppressionObserver)
		})
	}

	sel, err := metav1.LabelSelectorAsSelector(t.Selector)
	if err != nil {
		r.retries.remove(t.key())
		EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
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
	if err := r.patcher.RecyclePods(ctx, tw, t.Namespace, sel, recs, workload.WithTolerance(tol), workload.WithSuppressionObserver(suppressionObserver)); err != nil {
		return r.handleStepError(ctx, t, "patch", "Pod recycle failed", err)
	}

	r.recordStepSuccess(t)

	// Only report containers whose resources changed vs. the pod-template spec,
	// honouring the same downsize tolerance the patcher applies. This is a
	// best-effort approximation for the event: the patcher decides per live pod
	// (against each pod's actual resources), so in rollout edge cases the event
	// list can differ slightly from what was recycled.
	changed := changedContainers(containers, recs, tol)
	if len(changed) == 0 {
		logger.V(1).Info("recommendations match current resources, no event emitted")
		return nil
	}
	r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
		"Updated resources for containers: %v", changed)
	logger.Info("workload resources updated", "containers", changed)

	return nil
}

// resizeInPlaceTarget runs the reconcile tail shared by the CronJob and
// standalone Job paths. Both resize their currently-running pods in place and
// never evict them or mutate the workload spec, so the only kind-specific part
// is the pod enumeration + resize, supplied as resizeFn (it returns the number
// of pods the API server actually resized). The ResourcesUpdated event is only
// emitted when at least one pod was resized — the workload spec is never
// mutated, so changedContainers alone would fire on every reconcile.
func (r *PolicyReconciler) resizeInPlaceTarget(ctx context.Context, t *workloadTarget, containers []corev1.Container, recs map[string]workload.ContainerRecommendation, tol workload.Tolerance, resizeFn func() (int, error)) error {
	resized, err := resizeFn()
	if err != nil {
		return r.handleStepError(ctx, t, "resize", t.Kind+" pod resize failed", err)
	}
	r.recordStepSuccess(t)
	if resized > 0 {
		if changed := changedContainers(containers, recs, tol); len(changed) > 0 {
			r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
				"in-place resized %s pods for containers: %v", strings.ToLower(t.Kind), changed)
		}
	}
	return nil
}

// handleStepError applies the retry/event/metric policy for a failed reconcile
// step. Returns nil for non-transient errors (drop from retry tracker, no
// requeue) or err for transient ones (record attempt, schedule retry, emit
// event + metrics). phase labels EmitRetryState/IncrementRetryAttempt; msg is
// the human-readable description used in the event and log line.
func (r *PolicyReconciler) handleStepError(ctx context.Context, t *workloadTarget, phase, msg string, err error) error {
	if !isTransientError(err) {
		r.retries.remove(t.key())
		EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
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
	r.retries.recordFailure(t.key())
	state := r.retries.getState(t.key())
	r.recorder.Eventf(t.Object, nil, corev1.EventTypeWarning, "ReconciliationRetryScheduled", "ReconciliationRetryScheduled",
		"%s: %v. Retry attempt %d at %s", msg, err, state.attempts, state.nextRetry.Format(time.RFC3339))
	log.FromContext(ctx).Error(err, msg+", retry scheduled", "attempt", state.attempts)
	EmitRetryState(t.Namespace, t.Kind, t.Name, phase, true)
	IncrementRetryAttempt(t.Namespace, t.Kind, t.Name)
	return err
}

// recordStepSuccess clears retry state and emits the "no longer retrying"
// metric for a workload target whose latest step succeeded.
func (r *PolicyReconciler) recordStepSuccess(t *workloadTarget) {
	r.retries.recordSuccess(t.key())
	EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
}
