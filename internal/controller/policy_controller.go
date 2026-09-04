package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

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
	"github.com/noony/k8s-sustain/internal/oomwatch"
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
	PrometheusClient   *promclient.Client
	ReconcileInterval  time.Duration
	InPlaceUpdates     bool
	ExcludedNamespaces []string
	RecommendOnly      bool
	// WorkloadConcurrencyLimit caps how many workloads one Reconcile processes
	// in parallel.
	WorkloadConcurrencyLimit int
	// PolicyConcurrencyLimit caps how many Policy objects reconcile in parallel.
	PolicyConcurrencyLimit int

	// QueryShardMaxSamples bounds the per-shard sample budget (containers times
	// windowMinutes). Zero falls back to 10_000_000, a margin under Prometheus's
	// 50_000_000 default.
	QueryShardMaxSamples int

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
}

// LiveOOMConfig groups the inputs from the OOM Pod watcher. MaxAge zero means
// oomwatch.DefaultRecentMaxAge.
type LiveOOMConfig struct {
	Source    oomwatch.Source
	TriggerCh <-chan event.GenericEvent
	MaxAge    time.Duration
}

// Enabled reports whether both halves of the live-OOM path are wired.
func (c LiveOOMConfig) Enabled() bool {
	return c.Source != nil && c.TriggerCh != nil
}

// EffectiveMaxAge returns MaxAge or DefaultRecentMaxAge when MaxAge <= 0.
func (c LiveOOMConfig) EffectiveMaxAge() time.Duration {
	if c.MaxAge <= 0 {
		return oomwatch.DefaultRecentMaxAge
	}
	return c.MaxAge
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
	if r.QueryShardMaxSamples <= 0 {
		r.QueryShardMaxSamples = 10_000_000
	}
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

	targets, listErr := r.collectTargets(ctx, policy)
	if listErr != nil {
		logger.Error(listErr, "failed to list workloads")
		_ = r.failCondition(ctx, policy, "ListFailed", listErr)
		r.recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ListFailed", "ListFailed", "%s", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}
	logger.Info("collected workload targets", "count", len(targets))

	targetsByIdentity, discoveryFailures := r.discover(ctx, policy, targets)

	// Computation is driven by the WLR list, not the target list, so departed
	// identities are still recomputed.
	items, itemsErr := r.collectComputeItems(ctx, policy, targetsByIdentity)
	if itemsErr != nil {
		logger.Error(itemsErr, "failed to list WorkloadRecommendations for computation")
		_ = r.failCondition(ctx, policy, "ListFailed", itemsErr)
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}

	// Prefetch every identity's inputs in one sharded batch. The backoff decision
	// is taken once here and reused by the apply loop: shouldSkip is time-based
	// and the prefetch can take minutes, so asking twice would desync cands from
	// the processing loop. An identity is withheld only when every member is
	// backed off.
	skipBackoff := make(map[string]bool, len(targets))
	// pendingSnapshot marks identities withheld from cands for lack of a
	// snapshot, so buildRecommendations can tell that apart from a desync bug.
	pendingSnapshot := make(map[promclient.WorkloadIdentity]bool, len(items))
	// skipCompute marks identities whose members are all in backoff; computing
	// them would fall back to the per-workload queries backoff exists to suppress.
	skipCompute := make([]bool, len(items))
	cands := make([]promclient.ShardCandidate, 0, len(items))
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	for i := range items {
		it := &items[i]
		backedOff := 0
		for _, t := range it.Targets {
			if r.retries.shouldSkip(t.key()) {
				skipBackoff[t.key()] = true
				backedOff++
			}
		}
		if len(it.Targets) > 0 && backedOff == len(it.Targets) {
			skipCompute[i] = true
			continue
		}
		containers := containersFromObserved(it.Observed, excludeInit)
		if len(containers) == 0 {
			// No snapshot to size a shard with. Only candidacy is skipped; the identity
			// still runs below with nil inputs.
			pendingSnapshot[it.Identity] = true
			continue
		}
		cands = append(cands, promclient.ShardCandidate{
			Identity:   it.Identity,
			Containers: len(containers),
		})
	}
	batchInputs, batchStats := recommender.FetchWorkloadInputsBatch(ctx, r.PrometheusClient, cands,
		policy.Spec.RightSizing.ResourcesConfigs, r.QueryShardMaxSamples)

	// One autoscaler snapshot per pass; it lists each namespace once, lazily.
	autoSnap := autoscaler.NewNamespacedSnapshot(r.Client)

	var failCount atomic.Int32
	var skipped atomic.Int32
	// units counts dispatched work: one per workload object plus one per
	// departed identity.
	units := 0

	// Compute one recommendation per identity, in parallel, and finish before
	// any pod is touched so the webhook serves the new value before replacement
	// pods are admitted.
	recsByItem := make([]map[string]workload.ContainerRecommendation, len(items))
	errByItem := make([]error, len(items))
	cg, cgctx := errgroup.WithContext(ctx)
	cg.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range items {
		if skipCompute[i] {
			continue
		}
		it := items[i]
		inputs := batchInputs[it.Identity]
		// fetchErr is set only when both the shard query and the per-workload
		// fallback failed, so a Prometheus outage surfaces as a real error.
		fetchErr := batchStats.Failures[it.Identity]
		pending := pendingSnapshot[it.Identity]
		cg.Go(func() error {
			recsByItem[i], errByItem[i] = r.computeIdentity(cgctx, policy, it, autoSnap, inputs, fetchErr, pending)
			return nil // never cancel sibling goroutines
		})
	}
	_ = cg.Wait() // goroutines always return nil; per-identity errors ride errByItem

	// Departed identities are never applied, so their outcome is accounted for
	// here.
	for i := range items {
		if skipCompute[i] || len(items[i].Targets) > 0 {
			continue
		}
		units++
		if errByItem[i] != nil {
			failCount.Add(1)
		}
	}

	// Apply per workload object so a large owner-name group spreads across the
	// slots.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range items {
		it := items[i]
		recs, computeErr := recsByItem[i], errByItem[i]
		for _, t := range it.Targets {
			if skipBackoff[t.key()] {
				logger.V(1).Info("skipping workload in retry backoff", "target", t.key())
				skipped.Add(1)
				continue
			}
			units++
			g.Go(func() error {
				if err := r.reconcileWorkload(gctx, policy, t, autoSnap, recs, computeErr); err != nil {
					failCount.Add(1)
				}
				return nil // never cancel sibling goroutines
			})
		}
	}
	_ = g.Wait() // goroutines always return nil; errors are tracked via failCount

	logger.Info("reconcile cycle complete",
		"targets", len(targets),
		"identities", len(items),
		"dispatched", units,
		"discoveryFailures", discoveryFailures,
		"skipped", skipped.Load(),
		"failed", failCount.Load(),
		"concurrency", r.WorkloadConcurrencyLimit)

	keys := make([]string, 0, len(targets))
	for i := range targets {
		keys = append(keys, targets[i].key())
	}
	atRisk := r.retries.blockedCountAmong(keys)
	EmitPolicyRollup(policy.Name, len(targets), atRisk)

	// "resolved" means at least one sample came back, which a young workload on
	// a healthy Prometheus also fails; batchStats.Failures tells outages apart.
	resolved := 0
	for _, c := range cands {
		if wi := batchInputs[c.Identity]; wi != nil && (len(wi.CPUPerPod) > 0 || len(wi.MemPerPod) > 0) {
			resolved++
		}
	}
	EmitPolicyBatchCoverage(policy.Name, len(cands), resolved)
	EmitPolicyBatchFailures(policy.Name, len(batchStats.Failures))

	r.sweepWorkloadRecommendations(ctx, policy.Name, targets)

	// failCount is per dispatched unit, so units is the denominator.
	// discoveryFailures is reported separately so a persistent EnsureExists
	// failure cannot report Ready.
	failed := int(failCount.Load())
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
