package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=workloadrecommendations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
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
	ReconcileInterval  time.Duration
	InPlaceUpdates     bool
	ExcludedNamespaces []string
	RecommendOnly      bool
	// WorkloadConcurrencyLimit caps how many workloads one Reconcile processes
	// in parallel.
	WorkloadConcurrencyLimit int
	// PolicyConcurrencyLimit caps how many Policy objects reconcile in parallel.
	PolicyConcurrencyLimit int

	// Inputs fetches the recommendation inputs of a Policy's identities.
	Inputs recommender.InputsFetcher

	// RecycleReplacementTimeout caps how long the patcher waits for a replacement
	// pod to become Ready; it must cover node-autoscaling latency. Zero uses the
	// patcher default.
	RecycleReplacementTimeout time.Duration

	// RecommendationRetention is how long a WorkloadRecommendation outlives a
	// workload whose object is gone. Zero disables retention.
	RecommendationRetention time.Duration

	// OrphanReapInterval is how often orphaned WorkloadRecommendations are
	// reaped. Zero falls back to 10 minutes.
	OrphanReapInterval time.Duration

	// LiveOOM wires the OOM Pod-watcher path; a zero value disables it.
	LiveOOM LiveOOMConfig

	recorder events.EventRecorder
	patcher  *workload.Patcher
	retries  *retryTracker

	health healthTracker
}

// LiveOOMConfig groups the inputs from the OOM Pod watcher.
type LiveOOMConfig struct {
	Source    oomwatch.Source
	TriggerCh <-chan event.GenericEvent
}

// Enabled reports whether both halves of the live-OOM path are wired.
func (c LiveOOMConfig) Enabled() bool {
	return c.Source != nil && c.TriggerCh != nil
}

// SetupWithManager registers the PolicyReconciler with the given manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var patcherOpts []workload.Option
	if r.RecycleReplacementTimeout > 0 {
		patcherOpts = append(patcherOpts, workload.WithReadyTimeout(r.RecycleReplacementTimeout))
	}
	r.patcher = workload.New(r.Client, r.InPlaceUpdates, patcherOpts...)
	r.recorder = mgr.GetEventRecorder("k8s-sustain")
	r.retries = newRetryTracker()
	r.applyTuningDefaults()
	if err := mgr.Add(&orphanReaper{reconciler: r, interval: r.OrphanReapInterval}); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&sustainv1alpha1.Policy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.PolicyConcurrencyLimit})
	if r.LiveOOM.Enabled() {
		// An OOM event named after its policy enqueues that Policy immediately.
		b = b.WatchesRawSource(
			source.Channel(
				r.LiveOOM.TriggerCh,
				handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
					if obj == nil || obj.GetName() == "" {
						return nil
					}
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
				}),
			),
		)
	}
	return b.Complete(r)
}

// applyTuningDefaults fills in zero tuning knobs. The literals duplicate the
// CLI defaults rather than importing the config layer;
// TestSetupDefaultsAgreeWithConfigDefaults pins them together.
func (r *PolicyReconciler) applyTuningDefaults() {
	if r.WorkloadConcurrencyLimit <= 0 {
		r.WorkloadConcurrencyLimit = 5
	}
	if r.PolicyConcurrencyLimit <= 0 {
		r.PolicyConcurrencyLimit = 10
	}
}

// snapshot reads the identities in policy's namespaces and kinds. Every
// member of an identity shares its namespace and kind, so the narrowed
// snapshot still sees whole identities, and what makes one Conflicted.
func (r *PolicyReconciler) snapshot(ctx context.Context, policy *sustainv1alpha1.Policy) (*inventory.Snapshot, error) {
	var kinds []string
	for _, kind := range workload.SupportedKinds {
		if policy.Spec.RightSizing.Update.Types.ModeForKind(kind) != nil {
			kinds = append(kinds, kind)
		}
	}
	// An empty Kinds would read every kind.
	if len(kinds) == 0 {
		return &inventory.Snapshot{}, nil
	}
	return inventory.Take(ctx, r.Client, inventory.Options{
		Namespaces:         policy.Spec.Selector.Namespaces,
		Kinds:              kinds,
		ExcludedNamespaces: r.ExcludedNamespaces,
	})
}

// Reconcile is the main reconciliation loop for Policy objects.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("policy", req.Name)

	if r.Inputs == nil {
		return ctrl.Result{}, fmt.Errorf("recommendation inputs fetcher not configured")
	}

	policy := &sustainv1alpha1.Policy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.V(1).Info("policy fetched", "generation", policy.Generation, "resourceVersion", policy.ResourceVersion)

	// Cache cleanup runs before the finalizer is dropped so a transient failure
	// leaves the policy in place.
	const finalizerName = "k8s.sustain.io/cleanup"
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(policy, finalizerName) {
			if err := r.deleteAllRecommendationsForPolicy(ctx, policy.Name); err != nil {
				logger.Error(err, "failed to delete WorkloadRecommendations for policy; will retry")
				return ctrl.Result{}, err
			}
			// Ordered after cleanup so a retried deletion re-emits nothing.
			DeletePolicyMetrics(policy.Name)
			r.health.forget(policy.Name)
			r.recorder.Eventf(policy, nil, corev1.EventTypeNormal, "Cleanup", "Cleanup", "Policy deleted, removing finalizer.")
			controllerutil.RemoveFinalizer(policy, finalizerName)
			if err := r.Update(ctx, policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(policy, finalizerName) {
		controllerutil.AddFinalizer(policy, finalizerName)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	timer := prometheus.NewTimer(reconcileDuration.WithLabelValues(policy.Name))
	defer timer.ObserveDuration()

	logger.Info("starting reconcile cycle")

	snap, listErr := r.snapshot(ctx, policy)
	if listErr != nil {
		logger.Error(listErr, "failed to list workloads")
		_ = r.failCondition(ctx, policy, "ListFailed", listErr)
		r.recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ListFailed", "ListFailed", "%s", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}
	governed := snap.GovernedBy(policy.Name)
	logger.Info("collected governed identities", "count", len(governed))

	items := computeItems(ctx, policy, governed)
	discoveryFailures := r.discover(ctx, policy.Name, items)
	r.health.observe(policy.Name, items)
	r.recordConflicted(ctx, policy.Name, snap)

	// One autoscaler snapshot per pass; it lists each namespace once, lazily.
	autoSnap := autoscaler.NewNamespacedSnapshot(r.Client)

	// Every identity is computed and persisted before any pod is touched, so the
	// webhook serves the new value before replacement pods are admitted.
	results := r.recommend(ctx, policy, items, autoSnap)
	departed, departedFailed := r.persist(ctx, policy.Name, results)
	dispatched, failed, skipped := r.apply(ctx, policy, results, autoSnap)
	// units counts dispatched work: one per applied member plus one per
	// departed identity.
	units := dispatched + departed
	failed += departedFailed

	logger.Info("reconcile cycle complete",
		"identities", len(items),
		"dispatched", units,
		"discoveryFailures", discoveryFailures,
		"skipped", skipped,
		"failed", failed,
		"concurrency", r.WorkloadConcurrencyLimit)

	blocked := r.health.emit(policy.Name, r.retries)
	EmitPolicyRollup(policy.Name, liveIdentities(governed), blocked)

	requested, resolved, fetchFailures := passCoverage(results)
	EmitPolicyBatchCoverage(policy.Name, requested, resolved)
	EmitPolicyBatchFailures(policy.Name, fetchFailures)

	r.sweepWorkloadRecommendations(ctx, policy.Name, snap)

	// failed is per dispatched unit, so units is the denominator.
	// discoveryFailures is reported separately so a persistent EnsureExists
	// failure cannot report Ready.
	if failed > 0 || discoveryFailures > 0 {
		var parts []string
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d of %d workloads failed", failed, units))
		}
		if discoveryFailures > 0 {
			parts = append(parts, fmt.Sprintf("%d workload identities could not be registered for computation", discoveryFailures))
		}
		msg := strings.Join(parts, "; ")
		_ = r.failCondition(ctx, policy, "PartialFailure", errors.New(msg))
		r.recorder.Eventf(policy, nil, corev1.EventTypeWarning, "PartialFailure", "PartialFailure", "%s", msg)
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
	} else {
		msg := fmt.Sprintf("All %d workloads have been processed.", units)
		_ = r.setCondition(ctx, policy, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ReconciliationSucceeded",
			Message:            msg,
			ObservedGeneration: policy.Generation,
		})
		r.recorder.Eventf(policy, nil, corev1.EventTypeNormal, "ReconciliationSucceeded", "ReconciliationSucceeded",
			"%s", msg)
		reconcileTotal.WithLabelValues(policy.Name, "success").Inc()
	}

	return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
}

// liveIdentities counts the governed identities with a live member, which is
// what k8s_sustain_policy_workload_count reports.
func liveIdentities(governed []*inventory.Identity) int {
	n := 0
	for _, id := range governed {
		if !id.Departed() {
			n++
		}
	}
	return n
}
