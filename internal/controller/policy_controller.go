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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=patch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch

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
	recorder           record.EventRecorder
	patcher            *workload.Patcher
	retries            *retryTracker
}

// SetupWithManager registers the PolicyReconciler with the given manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.patcher = workload.New(r.Client, r.InPlaceUpdates)
	r.recorder = mgr.GetEventRecorderFor("k8s-sustain")
	r.retries = newRetryTracker()
	if r.ConcurrencyLimit <= 0 {
		r.ConcurrencyLimit = 5
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sustainv1alpha1.Policy{}).
		Complete(r)
}

// Reconcile is the main reconciliation loop for Policy objects.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.PrometheusClient == nil {
		return ctrl.Result{}, fmt.Errorf("prometheus client not configured")
	}

	policy := &sustainv1alpha1.Policy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: remove finalizer and let garbage collection clean up.
	const finalizerName = "k8s.sustain.io/cleanup"
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(policy, finalizerName) {
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

	// Collect all matching workload targets across all enabled kinds.
	targets, listErr := r.collectTargets(ctx, policy)
	if listErr != nil {
		_ = r.failCondition(ctx, policy, "ListFailed", listErr)
		r.recorder.Event(policy, corev1.EventTypeWarning, "ListFailed", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}

	// Process targets in parallel with bounded concurrency.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.ConcurrencyLimit)
	var failCount atomic.Int32

	for _, t := range targets {
		if r.retries.shouldSkip(t.key()) {
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
	types := policy.Spec.Update.Types
	namespaces := policy.Spec.Selector.Namespaces
	var targets []workloadTarget

	if types.Deployment != nil && *types.Deployment == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDeploymentTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing deployments: %w", err)
		}
		targets = append(targets, t...)
	}

	if types.StatefulSet != nil && *types.StatefulSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listStatefulSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing statefulsets: %w", err)
		}
		targets = append(targets, t...)
	}

	if types.DaemonSet != nil && *types.DaemonSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDaemonSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing daemonsets: %w", err)
		}
		targets = append(targets, t...)
	}

	if types.ArgoRollout != nil && *types.ArgoRollout == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listRolloutTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing rollouts: %w", err)
		}
		targets = append(targets, t...)
	}

	return filterTargets(targets, policy.Name, r.ExcludedNamespaces), nil
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

	recs, err := r.buildRecommendations(ctx, policy, t.Namespace, t.Kind, t.Name, t.Containers)
	if err != nil {
		if !isTransientError(err) {
			r.retries.remove(t.key())
			return nil
		}
		r.retries.recordFailure(t.key())
		state := r.retries.getState(t.key())
		r.recorder.Eventf(t.Object, corev1.EventTypeWarning, "ReconciliationRetryScheduled",
			"Prometheus query failed: %v. Retry attempt %d at %s", err, state.attempts, state.nextRetry.Format(time.RFC3339))
		logger.Error(err, "prometheus query failed, retry scheduled", "attempt", state.attempts)
		return err
	}

	if len(recs) == 0 {
		r.retries.recordSuccess(t.key())
		return nil
	}

	if r.RecommendOnly {
		logger.Info("recommend-only: computed recommendations", "recommendations", recs)
		r.retries.recordSuccess(t.key())
		return nil
	}

	sel, err := metav1.LabelSelectorAsSelector(t.Selector)
	if err != nil {
		r.retries.remove(t.key())
		return err
	}

	if err := r.patcher.RecyclePods(ctx, t.Namespace, sel, recs); err != nil {
		if !isTransientError(err) {
			r.retries.remove(t.key())
			return nil
		}
		r.retries.recordFailure(t.key())
		state := r.retries.getState(t.key())
		r.recorder.Eventf(t.Object, corev1.EventTypeWarning, "ReconciliationRetryScheduled",
			"Pod recycle failed: %v. Retry attempt %d at %s", err, state.attempts, state.nextRetry.Format(time.RFC3339))
		logger.Error(err, "pod recycle failed, retry scheduled", "attempt", state.attempts)
		return err
	}

	r.retries.recordSuccess(t.key())

	// Build a summary of the applied recommendations for the event message.
	var containers []string
	for name := range recs {
		containers = append(containers, name)
	}
	r.recorder.Eventf(t.Object, corev1.EventTypeNormal, "ResourcesUpdated",
		"Updated resources for containers: %v", containers)

	return nil
}

// buildRecommendations queries Prometheus and computes per-container recommendations
// for the given workload. Returns an empty map when no data is available yet.
func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	cpuQuantile := recommender.PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := recommender.ResourceWindow(rsCfg.CPU.Window)
	memQuantile := recommender.PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := recommender.ResourceWindow(rsCfg.Memory.Window)

	cpuValues, err := r.PrometheusClient.QueryCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}

	memValues, err := r.PrometheusClient.QueryMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		var rec workload.ContainerRecommendation
		hasData := false

		if cores, ok := cpuValues[c.Name]; ok {
			rec.CPURequest = recommender.ComputeCPURequest(cores, rsCfg.CPU.Requests)
			lr := recommender.ComputeLimit(rec.CPURequest, c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu(), rsCfg.CPU.Limits)
			rec.CPULimit = lr.Quantity
			rec.RemoveCPULimit = lr.Remove
			hasData = true
		}

		if bytes, ok := memValues[c.Name]; ok {
			rec.MemoryRequest = recommender.ComputeMemoryRequest(bytes, rsCfg.Memory.Requests)
			lr := recommender.ComputeLimit(rec.MemoryRequest, c.Resources.Requests.Memory(), c.Resources.Limits.Memory(), rsCfg.Memory.Limits)
			rec.MemoryLimit = lr.Quantity
			rec.RemoveMemoryLimit = lr.Remove
			hasData = true
		}

		if hasData {
			recs[c.Name] = rec
		}
	}

	return recs, nil
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
