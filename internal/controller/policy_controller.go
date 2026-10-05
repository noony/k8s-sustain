package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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

	// Inputs fetches the recommendation inputs of a Policy's identities,
	// live OOM kills included when it is decorated by recommender.WithLiveOOMs.
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

	// OOMTriggers carries a Policy as soon as the OOM Pod watcher sees one of
	// its pods killed, so it reconciles without waiting for the interval. Nil
	// disables it.
	OOMTriggers <-chan event.GenericEvent

	recorder events.EventRecorder
	patcher  *workload.Patcher
	ledger   *ledger

	health healthTracker
}

// SetupWithManager registers the PolicyReconciler with the given manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var patcherOpts []workload.Option
	if r.RecycleReplacementTimeout > 0 {
		patcherOpts = append(patcherOpts, workload.WithReadyTimeout(r.RecycleReplacementTimeout))
	}
	r.patcher = workload.New(r.Client, r.InPlaceUpdates, patcherOpts...)
	r.recorder = mgr.GetEventRecorder("k8s-sustain")
	r.ledger = newLedger(clock.RealClock{})
	r.applyTuningDefaults()
	if err := mgr.Add(&orphanReaper{reconciler: r, interval: r.OrphanReapInterval}); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&sustainv1alpha1.Policy{}, builder.WithPredicates(policyEvents)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.PolicyConcurrencyLimit})
	if r.OOMTriggers != nil {
		// An OOM event named after its policy enqueues that Policy immediately.
		b = b.WatchesRawSource(
			source.Channel(
				r.OOMTriggers,
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

// policyEvents passes the Policy events that call for a pass: creation,
// deletion and a new generation. The controller's own status and finalizer
// writes keep the generation and start none, so its status write cannot
// re-run a pass at once over the members it just backed off. Setting the
// deletion timestamp of an object holding finalizers bumps its generation,
// so cleanup still runs at once.
var policyEvents predicate.Predicate = predicate.GenerationChangedPredicate{}

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
			r.health.forget(policy.Name, r.ledger)
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

	snap, listErr := inventory.TakeForPolicy(ctx, r.Client, policy, r.ExcludedNamespaces)
	if listErr != nil {
		logger.Error(listErr, "failed to list workloads")
		_ = r.failCondition(ctx, policy, "ListFailed", listErr)
		r.recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ListFailed", "ListFailed", "%s", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}

	report := r.runPass(ctx, policy, snap)
	failed := r.settle(ctx, report)
	live := report.liveMembers()
	blocked := r.health.emit(policy.Name, live, r.ledger)
	EmitPolicyRollup(policy.Name, len(live), blocked)
	requested, resolved, fetchFailures := report.coverage()
	EmitPolicyBatchCoverage(policy.Name, requested, resolved)
	EmitPolicyBatchFailures(policy.Name, fetchFailures)

	r.sweepWorkloadRecommendations(ctx, policy.Name, snap)

	processed := report.processed()
	logger.Info("reconcile cycle complete",
		"identities", len(report),
		"processed", processed,
		"failed", failed,
		"blocked", blocked,
		"concurrency", r.WorkloadConcurrencyLimit)

	cond := passCondition(processed, failed, blocked)
	cond.ObservedGeneration = policy.Generation
	_ = r.setCondition(ctx, policy, cond)
	eventType, result := corev1.EventTypeNormal, "success"
	if cond.Status != metav1.ConditionTrue {
		eventType, result = corev1.EventTypeWarning, "error"
	}
	r.recorder.Eventf(policy, nil, eventType, cond.Reason, cond.Reason, "%s", cond.Message)
	reconcileTotal.WithLabelValues(policy.Name, result).Inc()

	return ctrl.Result{RequeueAfter: r.requeueAfter(live)}, nil
}

// settle hands every member's result in report to the ledger, emits the
// events and retry attempts it calls for, and returns how many identities
// failed this pass: a departed one whose decision could not be made or
// stored, or a live one with a member the ledger counts as failed.
func (r *PolicyReconciler) settle(ctx context.Context, report passReport) (failed int) {
	logger := log.FromContext(ctx)
	for _, rep := range report {
		identityFailed := rep.err != nil
		for _, res := range rep.members {
			ev, isFailure := r.ledger.record(ctx, res)
			if !isFailure {
				continue
			}
			identityFailed = true
			t := res.member
			IncrementRetryAttempt(t.identity())
			logger.Error(res.err, "applying the recommendation failed",
				"kind", t.Kind, "name", t.Name, "namespace", t.Namespace, "phase", res.phase)
			if ev != nil {
				r.recorder.Eventf(t.Object, nil, corev1.EventTypeWarning, ev.reason, ev.reason, "%s", ev.message)
			}
		}
		if identityFailed {
			failed++
		}
	}
	return failed
}

// passCondition is the Ready condition a pass leaves: True only when no
// identity failed this pass and none is Blocked, which also covers the
// members the pass skipped in backoff. Its counts are of identities, as every
// count of workloads is.
func passCondition(processed, failed, blocked int) metav1.Condition {
	if failed == 0 && blocked == 0 {
		return metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "ReconciliationSucceeded",
			Message: fmt.Sprintf("All %d workloads have been processed.", processed),
		}
	}
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d workloads failed", failed, processed))
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("Blocked workload identities: %d", blocked))
	}
	return metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  "PartialFailure",
		Message: strings.Join(parts, "; "),
	}
}

// requeueAfter schedules the Policy's next pass at the reconcile interval, or
// earlier when a retry of one of its live members falls due first, so the
// time a retry event names is a time a pass runs.
func (r *PolicyReconciler) requeueAfter(live map[promclient.WorkloadIdentity][]string) time.Duration {
	var members []string
	for _, keys := range live {
		members = append(members, keys...)
	}
	due, ok := r.ledger.nextRetry(members)
	if !ok {
		return r.ReconcileInterval
	}
	// A zero RequeueAfter means no requeue at all.
	return min(r.ReconcileInterval, max(due, time.Second))
}
