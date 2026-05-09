package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

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
	"github.com/noony/k8s-sustain/internal/workload"
)

// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
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
