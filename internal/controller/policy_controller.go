package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=patch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch

// PolicyReconciler reconciles a Policy object.
type PolicyReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	PrometheusClient   *promclient.Client
	ReconcileInterval  time.Duration
	InPlaceUpdates     bool
	ExcludedNamespaces []string
	RecommendOnly      bool
	ConcurrencyLimit   int

	// OrphanReapInterval bounds how often the manager scans for
	// WorkloadRecommendation objects whose owning Policy no longer exists
	// (strategy 2 cleanup). Zero falls back to 10 minutes.
	OrphanReapInterval time.Duration

	recorder record.EventRecorder
	patcher  *workload.Patcher
	retries  *retryTracker
}

// SetupWithManager registers the PolicyReconciler with the given manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.patcher = workload.New(r.Client, r.InPlaceUpdates)
	r.recorder = mgr.GetEventRecorderFor("k8s-sustain")
	r.retries = newRetryTracker()
	if r.ConcurrencyLimit <= 0 {
		r.ConcurrencyLimit = 5
	}
	if err := mgr.Add(&orphanReaper{reconciler: r, interval: r.OrphanReapInterval}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sustainv1alpha1.Policy{}).
		Complete(r)
}

// orphanReaper is a manager Runnable that periodically deletes
// WorkloadRecommendation objects whose owning Policy no longer exists.
// Strategy 2 cleanup — covers force-deleted policies, controller crashes
// mid-delete, and any other path that bypasses the per-policy finalizer.
type orphanReaper struct {
	reconciler *PolicyReconciler
	interval   time.Duration
}

func (o *orphanReaper) Start(ctx context.Context) error {
	interval := o.interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once at startup so a controller restart catches anything left over
	// while it was down.
	_ = o.reconciler.reapOrphanedRecommendations(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = o.reconciler.reapOrphanedRecommendations(ctx)
		}
	}
}

// NeedLeaderElection ensures only the leader runs the orphan reaper, so
// multi-replica controllers don't double-delete or race.
func (o *orphanReaper) NeedLeaderElection() bool { return true }

// Reconcile is the main reconciliation loop for Policy objects.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("policy", req.Name)

	if r.PrometheusClient == nil {
		return ctrl.Result{}, fmt.Errorf("prometheus client not configured")
	}

	policy := &sustainv1alpha1.Policy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("policy fetched", "generation", policy.Generation, "resourceVersion", policy.ResourceVersion)

	// Handle deletion: clean up cached recommendations, remove finalizer, and
	// let garbage collection take care of the policy itself. Cache cleanup
	// happens before the finalizer is dropped so a transient list/delete
	// failure leaves the policy in place — orphaned WLRs are then collected
	// by the periodic orphan reaper if the policy is force-deleted.
	const finalizerName = "k8s.sustain.io/cleanup"
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(policy, finalizerName) {
			if err := r.deleteAllRecommendationsForPolicy(ctx, policy.Name); err != nil {
				logger.Error(err, "failed to delete WorkloadRecommendations for policy; will retry")
				return ctrl.Result{}, err
			}
			r.recorder.Event(policy, corev1.EventTypeNormal, "Cleanup", "Policy deleted, removing finalizer.")
			controllerutil.RemoveFinalizer(policy, finalizerName)
			if err := r.Update(ctx, policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(policy, finalizerName) {
		controllerutil.AddFinalizer(policy, finalizerName)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	timer := prometheus.NewTimer(reconcileDuration.WithLabelValues(policy.Name))
	defer timer.ObserveDuration()

	logger.Info("starting reconcile cycle")

	// Collect all matching workload targets across all enabled kinds.
	targets, listErr := r.collectTargets(ctx, policy)
	if listErr != nil {
		logger.Error(listErr, "failed to list workloads")
		_ = r.failCondition(ctx, policy, "ListFailed", listErr)
		r.recorder.Event(policy, corev1.EventTypeWarning, "ListFailed", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}
	logger.Info("collected workload targets", "count", len(targets))

	// Process targets in parallel with bounded concurrency.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.ConcurrencyLimit)
	var failCount atomic.Int32
	var skipped atomic.Int32

	for _, t := range targets {
		if r.retries.shouldSkip(t.key()) {
			logger.V(1).Info("skipping workload in retry backoff", "target", t.key())
			skipped.Add(1)
			continue
		}
		g.Go(func() error {
			if err := r.reconcileWorkload(gctx, policy, &t); err != nil {
				failCount.Add(1)
			}
			return nil // never cancel sibling goroutines
		})
	}
	_ = g.Wait() // goroutines always return nil; errors are tracked via failCount

	logger.Info("reconcile cycle complete",
		"targets", len(targets),
		"skipped", skipped.Load(),
		"failed", failCount.Load(),
		"concurrency", r.ConcurrencyLimit)

	// Per-policy rollup: total matched workloads and how many are blocked in retry.
	keys := make([]string, 0, len(targets))
	for i := range targets {
		keys = append(keys, targets[i].key())
	}
	atRisk := r.retries.blockedCountAmong(keys)
	EmitPolicyRollup(policy.Name, len(targets), atRisk)

	// Sweep stale WorkloadRecommendations for this policy: any cached entry
	// whose target workload no longer exists (or is no longer matched by the
	// policy) is removed. Keeps etcd from accumulating dead cache entries.
	r.sweepWorkloadRecommendations(ctx, policy.Name, targets)

	failed := int(failCount.Load())
	if failed > 0 {
		msg := fmt.Sprintf("%d of %d workloads failed", failed, len(targets))
		_ = r.failCondition(ctx, policy, "PartialFailure", fmt.Errorf("%d of %d workloads failed", failed, len(targets)))
		r.recorder.Event(policy, corev1.EventTypeWarning, "PartialFailure", msg)
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
	} else {
		_ = r.setCondition(ctx, policy, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ReconciliationSucceeded",
			Message:            fmt.Sprintf("All %d targeted workloads have been processed.", len(targets)),
			ObservedGeneration: policy.Generation,
		})
		r.recorder.Event(policy, corev1.EventTypeNormal, "ReconciliationSucceeded",
			fmt.Sprintf("All %d targeted workloads have been processed.", len(targets)))
		reconcileTotal.WithLabelValues(policy.Name, "success").Inc()
	}

	return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
}

// collectTargets lists workloads of all enabled kinds and returns matching targets.
func (r *PolicyReconciler) collectTargets(ctx context.Context, policy *sustainv1alpha1.Policy) ([]workloadTarget, error) {
	logger := log.FromContext(ctx).WithValues("policy", policy.Name)
	types := policy.Spec.RightSizing.Update.Types
	namespaces := policy.Spec.Selector.Namespaces
	var targets []workloadTarget

	logger.V(1).Info("collecting targets",
		"namespaces", namespaces,
		"deployment", types.Deployment,
		"statefulset", types.StatefulSet,
		"daemonset", types.DaemonSet,
		"argoRollout", types.ArgoRollout)

	if types.Deployment != nil && *types.Deployment == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDeploymentTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing deployments: %w", err)
		}
		logger.V(1).Info("listed deployments", "count", len(t))
		targets = append(targets, t...)
	}

	if types.StatefulSet != nil && *types.StatefulSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listStatefulSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing statefulsets: %w", err)
		}
		logger.V(1).Info("listed statefulsets", "count", len(t))
		targets = append(targets, t...)
	}

	if types.DaemonSet != nil && *types.DaemonSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDaemonSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing daemonsets: %w", err)
		}
		logger.V(1).Info("listed daemonsets", "count", len(t))
		targets = append(targets, t...)
	}

	if types.ArgoRollout != nil && *types.ArgoRollout == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listRolloutTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing rollouts: %w", err)
		}
		logger.V(1).Info("listed argo rollouts", "count", len(t))
		targets = append(targets, t...)
	}

	filtered := filterTargets(targets, policy.Name, r.ExcludedNamespaces)
	logger.V(1).Info("filtered targets",
		"raw", len(targets),
		"matching", len(filtered),
		"excludedNamespaces", r.ExcludedNamespaces)
	return filtered, nil
}

// listDeploymentTargets lists Deployments, scoped to namespaces if provided.
func (r *PolicyReconciler) listDeploymentTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	var targets []workloadTarget

	if len(namespaces) > 0 {
		for _, ns := range namespaces {
			var list appsv1.DeploymentList
			if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			for i := range list.Items {
				targets = append(targets, deploymentToTarget(&list.Items[i]))
			}
		}
		return targets, nil
	}

	var list appsv1.DeploymentList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		targets = append(targets, deploymentToTarget(&list.Items[i]))
	}
	return targets, nil
}

// listStatefulSetTargets lists StatefulSets, scoped to namespaces if provided.
func (r *PolicyReconciler) listStatefulSetTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	var targets []workloadTarget

	if len(namespaces) > 0 {
		for _, ns := range namespaces {
			var list appsv1.StatefulSetList
			if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			for i := range list.Items {
				targets = append(targets, statefulSetToTarget(&list.Items[i]))
			}
		}
		return targets, nil
	}

	var list appsv1.StatefulSetList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		targets = append(targets, statefulSetToTarget(&list.Items[i]))
	}
	return targets, nil
}

// listDaemonSetTargets lists DaemonSets, scoped to namespaces if provided.
func (r *PolicyReconciler) listDaemonSetTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	var targets []workloadTarget

	if len(namespaces) > 0 {
		for _, ns := range namespaces {
			var list appsv1.DaemonSetList
			if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			for i := range list.Items {
				targets = append(targets, daemonSetToTarget(&list.Items[i]))
			}
		}
		return targets, nil
	}

	var list appsv1.DaemonSetList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		targets = append(targets, daemonSetToTarget(&list.Items[i]))
	}
	return targets, nil
}

// listRolloutTargets lists Argo Rollouts, scoped to namespaces if provided.
func (r *PolicyReconciler) listRolloutTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	var targets []workloadTarget

	if len(namespaces) > 0 {
		for _, ns := range namespaces {
			var list rolloutsv1alpha1.RolloutList
			if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			for i := range list.Items {
				targets = append(targets, rolloutToTarget(&list.Items[i]))
			}
		}
		return targets, nil
	}

	var list rolloutsv1alpha1.RolloutList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		targets = append(targets, rolloutToTarget(&list.Items[i]))
	}
	return targets, nil
}

// reconcileWorkload processes a single workload target: queries Prometheus,
// computes recommendations, recycles pods, emits events, and tracks retries.
func (r *PolicyReconciler) reconcileWorkload(ctx context.Context, policy *sustainv1alpha1.Policy, t *workloadTarget) error {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)
	logger.V(1).Info("reconciling workload", "containers", len(t.Containers))

	// Detect HPA / KEDA ScaledObject (read-only). Used as a replica-count
	// fallback for workload-level recommendations and for observability.
	autoInfo, autoErr := autoscaler.Detect(ctx, r.Client, t.Namespace, t.Kind, t.Name)
	if autoErr != nil {
		logger.Error(autoErr, "autoscaler detection failed, proceeding without it")
		autoInfo = autoscaler.Info{Kind: autoscaler.KindNone}
	}
	if autoInfo.Kind != autoscaler.KindNone {
		r.recorder.Eventf(t.Object, corev1.EventTypeNormal, "AutoscalerDetected",
			"%s %s detected targeting %s/%s (replicas %d–%d)",
			autoInfo.Kind, autoInfo.Name, t.Kind, t.Name, autoInfo.MinReplicas, autoInfo.MaxReplicas)
	}
	EmitAutoscalerPresent(t.Namespace, t.Kind, t.Name, string(autoInfo.Kind))
	EmitAutoscalerTargetsConfigured(t.Namespace, t.Kind, t.Name, string(autoInfo.Kind), autoInfo.ConfiguredTargets)

	recs, err := r.buildRecommendations(ctx, policy, t.Namespace, t.Kind, t.Name, t.Containers, autoInfo)
	if err != nil {
		if !isTransientError(err) {
			r.retries.remove(t.key())
			EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
			return nil
		}
		r.retries.recordFailure(t.key())
		state := r.retries.getState(t.key())
		r.recorder.Eventf(t.Object, corev1.EventTypeWarning, "ReconciliationRetryScheduled",
			"Prometheus query failed: %v. Retry attempt %d at %s", err, state.attempts, state.nextRetry.Format(time.RFC3339))
		logger.Error(err, "prometheus query failed, retry scheduled", "attempt", state.attempts)
		EmitRetryState(t.Namespace, t.Kind, t.Name, "prometheus", true)
		IncrementRetryAttempt(t.Namespace, t.Kind, t.Name)
		return err
	}

	if len(recs) == 0 {
		logger.V(1).Info("no recommendations available yet (no Prometheus data)")
		r.retries.recordSuccess(t.key())
		EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
		return nil
	}

	logger.Info("computed recommendations", "containers", len(recs))
	logger.V(1).Info("recommendation details", "recommendations", recs)

	// Emit per-container recommendation/drift metrics before recycling pods.
	emitWorkloadFromRecs(t, policy.Name, recs)

	// Persist last-known-good recommendation as a WorkloadRecommendation.
	// Lets the webhook serve cached values during a Prometheus outage and
	// gives operators a `kubectl get wlrec` audit surface. Best-effort: the
	// upsert never propagates errors so a failed cache write can't block the
	// recycle path.
	r.upsertWorkloadRecommendation(ctx, t, policy.Name, recs, metav1.Now())

	if r.RecommendOnly {
		logger.Info("recommend-only: computed recommendations", "recommendations", recs)
		r.retries.recordSuccess(t.key())
		EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
		return nil
	}

	sel, err := metav1.LabelSelectorAsSelector(t.Selector)
	if err != nil {
		r.retries.remove(t.key())
		EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
		return err
	}

	logger.V(1).Info("recycling pods", "selector", sel.String())
	if err := r.patcher.RecyclePods(ctx, t.Namespace, sel, recs); err != nil {
		if !isTransientError(err) {
			r.retries.remove(t.key())
			EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)
			return nil
		}
		r.retries.recordFailure(t.key())
		state := r.retries.getState(t.key())
		r.recorder.Eventf(t.Object, corev1.EventTypeWarning, "ReconciliationRetryScheduled",
			"Pod recycle failed: %v. Retry attempt %d at %s", err, state.attempts, state.nextRetry.Format(time.RFC3339))
		logger.Error(err, "pod recycle failed, retry scheduled", "attempt", state.attempts)
		EmitRetryState(t.Namespace, t.Kind, t.Name, "patch", true)
		IncrementRetryAttempt(t.Namespace, t.Kind, t.Name)
		return err
	}

	r.retries.recordSuccess(t.key())
	EmitRetryState(t.Namespace, t.Kind, t.Name, "", false)

	// Only report containers whose resources actually changed vs. the current spec.
	changed := changedContainers(t.Containers, recs)
	if len(changed) == 0 {
		logger.V(1).Info("recommendations match current resources, no event emitted")
		return nil
	}
	r.recorder.Eventf(t.Object, corev1.EventTypeNormal, "ResourcesUpdated",
		"Updated resources for containers: %v", changed)
	logger.Info("workload resources updated", "containers", changed)

	return nil
}

// changedContainers returns the names of containers whose recommended CPU/memory
// requests or limits differ from the current spec. A nil/zero quantity in either
// side is treated as "unset" and matches another unset value.
func changedContainers(current []corev1.Container, recs map[string]workload.ContainerRecommendation) []string {
	byName := make(map[string]corev1.Container, len(current))
	for _, c := range current {
		byName[c.Name] = c
	}
	var changed []string
	for name, rec := range recs {
		c, ok := byName[name]
		if !ok {
			changed = append(changed, name)
			continue
		}
		if !requestEqual(rec.CPURequest, c.Resources.Requests.Cpu()) ||
			!requestEqual(rec.MemoryRequest, c.Resources.Requests.Memory()) ||
			!limitEqual(rec.CPULimit, rec.RemoveCPULimit, c.Resources.Limits.Cpu()) ||
			!limitEqual(rec.MemoryLimit, rec.RemoveMemoryLimit, c.Resources.Limits.Memory()) {
			changed = append(changed, name)
		}
	}
	return changed
}

// requestEqual reports whether the recommendation matches the current request,
// treating a nil recommendation as "leave it alone" (i.e. unchanged) since the
// patcher takes no action in that case.
func requestEqual(rec *resource.Quantity, current *resource.Quantity) bool {
	if rec == nil {
		return true
	}
	return quantityEqual(rec, current)
}

func quantityEqual(a *resource.Quantity, b *resource.Quantity) bool {
	aZero := a == nil || a.IsZero()
	bZero := b == nil || b.IsZero()
	if aZero && bZero {
		return true
	}
	if aZero != bZero {
		return false
	}
	return a.Cmp(*b) == 0
}

// limitEqual reports whether the limit recommendation matches the current
// limit. A nil rec without remove=true means "leave it alone" → unchanged.
func limitEqual(rec *resource.Quantity, remove bool, current *resource.Quantity) bool {
	currentZero := current == nil || current.IsZero()
	if remove {
		return currentZero
	}
	if rec == nil {
		return true
	}
	return quantityEqual(rec, current)
}

// buildRecommendations queries Prometheus for the workload-level CPU/memory totals
// and replica count, then derives per-container per-pod recommendations.
// A per-pod p95 floor is applied to protect against load imbalance.
// autoInfo provides the autoscaler MinReplicas fallback used when Prometheus has
// no replica data (KEDA scale-to-zero, missing samples).
func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
	autoInfo autoscaler.Info,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	cpuQuantile := recommender.PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := recommender.ResourceWindow(rsCfg.CPU.Window)
	memQuantile := recommender.PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := recommender.ResourceWindow(rsCfg.Memory.Window)

	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)
	logger.V(1).Info("querying Prometheus (workload-level)",
		"cpuQuantile", cpuQuantile, "cpuWindow", cpuWindow,
		"memQuantile", memQuantile, "memWindow", memWindow)

	cpuTotals, err := r.PrometheusClient.QueryWorkloadCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("workload cpu query: %w", err)
	}
	memTotals, err := r.PrometheusClient.QueryWorkloadMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("workload memory query: %w", err)
	}

	// Per-pod p95 floors used for hot-replica protection. A failure here is
	// non-fatal: we still produce recommendations from the workload-level data.
	cpuFloors, err := r.PrometheusClient.QueryCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		logger.V(1).Info("per-pod cpu floor query failed; proceeding without floor", "err", err)
		cpuFloors = nil
	}
	memFloors, err := r.PrometheusClient.QueryMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		logger.V(1).Info("per-pod memory floor query failed; proceeding without floor", "err", err)
		memFloors = nil
	}

	medianReplicas, err := r.PrometheusClient.QueryReplicaCountMedian(ctx, ns, ownerKind, ownerName, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("replica count query: %w", err)
	}
	replicas := recommender.EffectiveReplicas(medianReplicas, autoInfo.MinReplicas)
	logger.V(1).Info("effective replica divisor",
		"medianReplicas", medianReplicas, "autoMinReplicas", autoInfo.MinReplicas, "effective", replicas)

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination
	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		var rec workload.ContainerRecommendation
		hasData := false

		if total, ok := cpuTotals[c.Name]; ok {
			perPod := recommender.PerPodFromTotal(total, replicas)
			perPod = recommender.ApplyFloor(perPod, cpuFloors[c.Name])
			rec.CPURequest = recommender.ComputeCPURequest(perPod, rsCfg.CPU.Requests)
			logger.V(1).Info("computed CPU recommendation",
				"container", c.Name, "totalCores", total, "replicas", replicas,
				"perPodCores", perPod, "request", quantityString(rec.CPURequest))
			hasData = true
		}

		if total, ok := memTotals[c.Name]; ok {
			perPod := recommender.PerPodFromTotal(total, replicas)
			perPod = recommender.ApplyFloor(perPod, memFloors[c.Name])
			rec.MemoryRequest = recommender.ComputeMemoryRequest(perPod, rsCfg.Memory.Requests)
			logger.V(1).Info("computed memory recommendation",
				"container", c.Name, "totalBytes", total, "replicas", replicas,
				"perPodBytes", perPod, "request", quantityString(rec.MemoryRequest))
			hasData = true
		}

		if !hasData {
			continue
		}

		// Apply autoscaler coordination (overhead + replica budget) before
		// limits are derived, so limits track the adjusted requests.
		base := rec
		rec = recommender.ApplyCoordination(rec, coordCfg, autoInfo, rsCfg)
		emitCoordinationFactors(ns, ownerKind, ownerName, coordCfg, autoInfo, base, rec)

		// Re-derive limits from the (possibly) adjusted requests.
		if rec.CPURequest != nil {
			lr := recommender.ComputeLimit(rec.CPURequest, c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu(), rsCfg.CPU.Limits)
			rec.CPULimit = lr.Quantity
			rec.RemoveCPULimit = lr.Remove
		}
		if rec.MemoryRequest != nil {
			lr := recommender.ComputeLimit(rec.MemoryRequest, c.Resources.Requests.Memory(), c.Resources.Limits.Memory(), rsCfg.Memory.Limits)
			rec.MemoryLimit = lr.Quantity
			rec.RemoveMemoryLimit = lr.Remove
		}

		recs[c.Name] = rec
	}
	return recs, nil
}

// factorRatio returns adjusted/baseline as a float64. Returns 1.0 (no-op
// signal) when either side is nil or the baseline is zero, so the metric
// never emits NaN/Inf.
func factorRatio(adjusted, baseline *resource.Quantity) float64 {
	if adjusted == nil || baseline == nil || baseline.IsZero() {
		return 1.0
	}
	return float64(adjusted.MilliValue()) / float64(baseline.MilliValue())
}

// emitCoordinationFactors records overhead and (CPU only) replica multipliers
// applied by ApplyCoordination, decomposed for dashboard rendering. No-op when
// coordination is disabled or no autoscaler targets the workload.
func emitCoordinationFactors(
	namespace, ownerKind, ownerName string,
	cfg sustainv1alpha1.AutoscalerCoordination,
	info autoscaler.Info,
	base, adjusted workload.ContainerRecommendation,
) {
	if !cfg.Enabled || info.Kind == autoscaler.KindNone {
		return
	}

	// CPU: overhead-only ratio computed independently so we can split it from
	// the replica correction in the same metric family. Total = overhead × replica.
	if base.CPURequest != nil {
		cpuOverhead := recommender.ApplyOverhead(base.CPURequest, info.ConfiguredTargets[autoscaler.ResourceCPU])
		overheadFactor := factorRatio(cpuOverhead, base.CPURequest)
		EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceCPU, "overhead", overheadFactor)
		if cfg.ReplicaBudgetAnchor != nil {
			totalFactor := factorRatio(adjusted.CPURequest, base.CPURequest)
			replicaFactor := 1.0
			if overheadFactor != 0 {
				replicaFactor = totalFactor / overheadFactor
			}
			EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceCPU, "replica", replicaFactor)
		}
	}

	// Memory: overhead only.
	if base.MemoryRequest != nil {
		EmitCoordinationFactor(namespace, ownerKind, ownerName, autoscaler.ResourceMemory, "overhead",
			factorRatio(adjusted.MemoryRequest, base.MemoryRequest))
	}
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return "<nil>"
	}
	return q.String()
}

// setCondition upserts a status condition on policy, preserving LastTransitionTime
// when the status is unchanged, then persists via the status subresource.
func (r *PolicyReconciler) setCondition(ctx context.Context, policy *sustainv1alpha1.Policy, cond metav1.Condition) error {
	cond.LastTransitionTime = metav1.Now()

	for i, c := range policy.Status.Conditions {
		if c.Type != cond.Type {
			continue
		}
		if c.Status == cond.Status {
			cond.LastTransitionTime = c.LastTransitionTime
		}
		policy.Status.Conditions[i] = cond
		return r.Status().Update(ctx, policy)
	}

	policy.Status.Conditions = append(policy.Status.Conditions, cond)
	return r.Status().Update(ctx, policy)
}

// failCondition sets a Ready=False condition and returns the original error so the
// caller can propagate it to the controller-runtime retry machinery.
func (r *PolicyReconciler) failCondition(ctx context.Context, policy *sustainv1alpha1.Policy, reason string, err error) error {
	_ = r.setCondition(ctx, policy, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            err.Error(),
		ObservedGeneration: policy.Generation,
	})
	return err
}
