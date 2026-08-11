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
	// WorkloadConcurrencyLimit caps how many workloads a single Policy's
	// Reconcile processes in parallel.
	WorkloadConcurrencyLimit int
	// PolicyConcurrencyLimit caps how many distinct Policy objects are
	// reconciled in parallel. Concurrent Reconcile calls share only the
	// mutex-protected retryTracker (already safe for concurrent access from
	// the existing per-workload errgroup fan-out within a single Reconcile);
	// autoscaler.NamespacedSnapshot is a fresh, non-shared instance built
	// per Reconcile call, so it carries no cross-Policy sharing risk at all.
	PolicyConcurrencyLimit int

	// QueryShardMaxSamples bounds the per-shard sample budget passed to
	// recommender.FetchWorkloadInputsBatch (see promclient.BuildShards):
	// containers * windowMinutes summed across a shard's members must stay
	// under this before a new shard is started, keeping each batched query
	// under Prometheus's --query.max-samples. Zero falls back to 10_000_000
	// (a safety margin under Prometheus's own 50_000_000 default). Wired from
	// the --query-shard-max-samples flag (config.DefaultQueryShardMaxSamples)
	// in cmd/controller/start.go.
	QueryShardMaxSamples int

	// RecycleReplacementTimeout caps how long the patcher waits for a
	// replacement pod to become Ready after an eviction. Must be generous
	// enough to cover node-autoscaling latency on the slowest expected
	// cluster (Karpenter / CAS provisioning can take several minutes); too
	// short produces false-positive aborts during normal scale-up. Zero
	// falls back to the patcher default.
	RecycleReplacementTimeout time.Duration

	// RecommendationRetention is how long a WorkloadRecommendation outlives a
	// workload whose object has disappeared (ephemeral bare pods, deleted or
	// terminal Jobs). The dashboard shows these as inactive rows. Zero
	// disables retention: departed targets are swept on the next reconcile
	// (after the fresh-write grace period).
	RecommendationRetention time.Duration

	// OrphanReapInterval bounds how often the manager scans for
	// WorkloadRecommendation objects whose owning Policy no longer exists
	// (strategy 2 cleanup). Zero falls back to 10 minutes.
	OrphanReapInterval time.Duration

	// LiveOOM bundles the dependencies for the active Pod-watcher path.
	// A zero value (Source nil, TriggerCh nil) disables it; both fields are
	// expected to be wired as a pair.
	LiveOOM LiveOOMConfig

	recorder events.EventRecorder
	patcher  *workload.Patcher
	retries  *retryTracker
}

// LiveOOMConfig groups the inputs from the active OOM Pod watcher. Source
// feeds the recommender; TriggerCh enqueues affected Policies on each
// observed kill; MaxAge bounds how recent a cache record must be to count as
// a live signal (zero means oomwatch.DefaultRecentMaxAge).
type LiveOOMConfig struct {
	Source    oomwatch.Source
	TriggerCh <-chan event.GenericEvent
	MaxAge    time.Duration
}

// Enabled reports whether both halves of the live-OOM path are wired.
// Partial wiring is treated as disabled so a caller that forgets one field
// doesn't get a half-active feature.
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
		// The watcher feeds synthetic Policy GenericEvents (Name = policy
		// annotation value). The map func turns each into a reconcile.Request,
		// so an observed OOM enqueues the owning Policy immediately.
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

// applyTuningDefaults fills in the tuning knobs a reconciler constructed
// without them would otherwise leave at zero — which for every one of these
// would be actively harmful rather than merely unset (a zero concurrency limit
// stalls, a zero sample budget produces degenerate one-workload shards).
//
// These literals intentionally duplicate the CLI defaults in internal/config
// rather than importing them: the reconciler has no business depending on the
// flag/Viper layer, which exists a level above it and is only ever the caller.
// TestSetupDefaultsAgreeWithConfigDefaults pins the two together so the
// duplication cannot silently drift.
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
			// After the WLRs are gone, drop the policy's metric series too.
			// Ordered after the cleanup above so a retried deletion re-emits
			// nothing: by this point no further reconcile will run for it.
			DeletePolicyMetrics(policy.Name)
			r.recorder.Eventf(policy, nil, corev1.EventTypeNormal, "Cleanup", "Cleanup", "Policy deleted, removing finalizer.")
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
		r.recorder.Eventf(policy, nil, corev1.EventTypeWarning, "ListFailed", "ListFailed", "%s", listErr.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}
	logger.Info("collected workload targets", "count", len(targets))

	// Phase 1 — discovery. Index the listing by identity and guarantee every
	// matched identity has a WorkloadRecommendation before computation runs.
	// No Prometheus queries happen here.
	targetsByIdentity, discoveryFailures := r.discover(ctx, policy, targets)

	// Phase 2 — computation. Driven by the WorkloadRecommendation list, not the
	// target listing, so an identity absent from the listing (a completed Job,
	// a bare-pod group between runs) is still recomputed every cycle.
	items, itemsErr := r.collectComputeItems(ctx, policy, targetsByIdentity)
	if itemsErr != nil {
		logger.Error(itemsErr, "failed to list WorkloadRecommendations for computation")
		_ = r.failCondition(ctx, policy, "ListFailed", itemsErr)
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
	}

	// Prefetch every identity's Prometheus inputs in ONE sharded batch call,
	// before the per-workload errgroup below even starts. This is the actual
	// payoff of the whole batching phase: collapsing the ~3-4 Prometheus
	// round trips PER WORKLOAD the pipeline used to issue down to
	// a handful of shard queries for the entire policy, regardless of how
	// many workloads it matches. Every kind batches identically now — "Job"
	// and "Pod" identities used to be withheld here because they needed
	// WorkloadInputs.HistoryStart, whose mandatory PromQL subquery could not
	// be sharded; the age gate now reads the identity's WorkloadRecommendation
	// CreationTimestamp instead and that query is gone, so nothing kind-specific
	// stops them from batching like every other kind.
	//
	// The retry-backoff decision is taken ONCE here and reused by the processing
	// loop below, rather than being re-evaluated there.
	//
	// shouldSkip is purely time-based (time.Now().Before(s.nextRetry)), and the
	// batch prefetch sits between the two loops taking seconds to minutes. Asked
	// twice, a workload whose backoff window expires in that gap answers "skip"
	// first and "process" second: it is left out of cands, so batchInputs has no
	// entry for it, and the processing loop then hands buildRecommendations a nil
	// inputs that is NOT one this loop deliberately withheld — tripping the
	// errPrefetchMissingForBatchedKind branch, whose message tells operators to
	// investigate a bug in this very loop. With 30s–5m backoff windows that fires
	// routinely on any cluster with failing workloads, pointing at nothing.
	//
	// Applying backoff to candidacy at all is deliberate: including a backed-off
	// workload is nearly free on the shard path, but when a shard query fails
	// twice, fallbackShardPerWorkload issues a per-workload FetchWorkloadInputs
	// for EVERY name in that shard. Without the filter that reintroduces the full
	// pre-batching query volume precisely when Prometheus is unhealthy — and
	// backoff exists to suppress exactly those queries.
	//
	// Backoff is keyed on the real workload object, not the identity, because
	// that is what records failures (r.retries is keyed by
	// workloadTarget.key()) and what the apply phase acts on. Under owner-name
	// grouping one member can be backed off while its sibling is due, so an
	// identity is withheld from the batch only when EVERY member is backed off
	// — withholding it because of one sick member would deny the healthy ones
	// their inputs. A departed identity has no target and therefore no backoff
	// state: it only ever reads Prometheus and writes its own cache object, so
	// there is no failing apply to back off from.
	//
	// Evaluated for every identity, before the empty-snapshot candidacy check
	// below, so the processing loop has an answer either way. Written only here
	// and read-only afterwards, so the parallel section below needs no lock.
	skipBackoff := make(map[string]bool, len(targets))
	// pendingSnapshot records identities this loop itself withheld from cands
	// because containersFromObserved found nothing to size a shard with (see
	// the continue below) -- a legitimate, expected state, not a bug. It is
	// threaded down to buildRecommendations so its nil-inputs guard can tell
	// that apart from a genuine desync between this loop and BatchInputs,
	// which is the only thing errPrefetchMissingForBatchedKind exists to
	// catch.
	pendingSnapshot := make(map[promclient.WorkloadIdentity]bool, len(items))
	// skipCompute[i] marks an identity every one of whose members is in retry
	// backoff. It is recorded here rather than re-derived because the
	// computation phase below must make exactly the same decision this loop
	// did: an identity left out of cands has no batched inputs, so computing it
	// anyway would fall back to a per-workload Prometheus query — reintroducing
	// the query volume backoff exists to suppress, precisely when Prometheus is
	// unhealthy. Departed identities have no members and are never skipped.
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
			// No container list to size a shard with. For a live identity that
			// cannot happen unless its members declare no recommendable
			// container at all (every container an initContainer, with
			// excludeInitContainers set) — computeItem.Observed is built from
			// the members' own pod templates. For a departed one it means
			// nothing ever snapshotted the identity (a webhook stub create
			// whose status write failed, an EnsureExists that failed). Skipping
			// only CANDIDACY, not processing — the identity still runs below
			// with a nil inputs, where a live target falls back to a
			// per-workload fetch and a departed one returns early.
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

	// Build autoscaler discovery ONCE per reconcile pass instead of per
	// workload. Targets may span multiple namespaces, so this lazily lists
	// HPAs + ScaledObjects per namespace on first lookup and caches the result,
	// turning the former 2·M namespace-wide List calls into 2·(distinct
	// namespaces). Lookups are concurrency-safe.
	autoSnap := autoscaler.NewNamespacedSnapshot(r.Client)

	var failCount atomic.Int32
	var skipped atomic.Int32
	// units counts the work actually dispatched: one per real workload object,
	// plus one per departed identity. It is the honest denominator for the
	// failure rollup below, since failCount is incremented per dispatched unit
	// and owner-name grouping makes identities and objects differ.
	units := 0

	// Computation — ONE recommendation per identity, in parallel. Splitting it
	// out of the apply loop is what stops the members of an owner-name group
	// producing competing answers for the single object they share: they read
	// the same Prometheus series and write the same WorkloadRecommendation, so
	// computing per member left the group's stored recommendation (and its
	// observed-resources snapshot) to be decided by whichever goroutine
	// finished last. See computeIdentity.
	//
	// It completes before any pod is touched, so every WorkloadRecommendation
	// write of the cycle lands before the first eviction — the webhook is
	// serving the new value by the time replacement pods are admitted.
	recsByItem := make([]map[string]workload.ContainerRecommendation, len(items))
	errByItem := make([]error, len(items))
	cg, cgctx := errgroup.WithContext(ctx)
	cg.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range items {
		if skipCompute[i] {
			continue
		}
		it := items[i]
		// A missing entry here means this identity had no observed-resources
		// snapshot yet when candidacy was built above (see pendingSnapshot) --
		// inputs stays nil and buildRecommendations falls back to its own
		// per-workload fetch.
		inputs := batchInputs[it.Identity]
		// fetchErr is non-nil only when the batch's shard query AND its
		// per-workload fallback both genuinely failed to reach Prometheus for
		// this identity (recommender.BatchStats.Failures) -- e.g. a total
		// outage. Threading it through restores the pre-batching behaviour of
		// a Prometheus failure surfacing as a real error (retry tracking,
		// ReconciliationRetryScheduled event, PartialFailure condition)
		// instead of silently degrading to an empty, "successful"
		// recommendation. See buildRecommendations for where it is consumed.
		fetchErr := batchStats.Failures[it.Identity]
		// pending tells buildRecommendations whether a nil inputs above is
		// this loop's own deliberate withholding (pendingSnapshot) rather than
		// a desync bug -- see pendingSnapshot's doc comment.
		pending := pendingSnapshot[it.Identity]
		cg.Go(func() error {
			recsByItem[i], errByItem[i] = r.computeIdentity(cgctx, policy, it, autoSnap, inputs, fetchErr, pending)
			return nil // never cancel sibling goroutines
		})
	}
	_ = cg.Wait() // goroutines always return nil; per-identity errors ride errByItem

	// Departed identities are computed and cached but never applied — there are
	// no pods to align — so the apply loop below never sees them and their
	// outcome is accounted for here instead. A live identity's computation
	// failure is accounted for per member, where retry state lives.
	for i := range items {
		if skipCompute[i] || len(items[i].Targets) > 0 {
			continue
		}
		units++
		if errByItem[i] != nil {
			failCount.Add(1)
		}
	}

	// Apply in parallel with bounded concurrency. The unit of parallelism is
	// the real workload object, not the identity, so a large owner-name group
	// still spreads across the available slots.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range items {
		it := items[i]
		recs, computeErr := recsByItem[i], errByItem[i]
		// One unit per real workload object behind the identity. Under
		// owner-name grouping that is every member of the group, each aligning
		// its OWN pods against the one shared recommendation. Dispatching per
		// member rather than looping inside a single goroutine keeps the
		// per-object parallelism the errgroup limit is sized for — an Airflow
		// owner-name group can hold dozens of members.
		for _, t := range it.Targets {
			// Reuses the decision taken before the prefetch — see skipBackoff.
			// skipCompute[i] is the same decision for every member at once, so
			// an identity that was not computed dispatches nothing here.
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

	// Per-policy rollup: total matched workloads and how many are blocked in retry.
	keys := make([]string, 0, len(targets))
	for i := range targets {
		keys = append(keys, targets[i].key())
	}
	atRisk := r.retries.blockedCountAmong(keys)
	EmitPolicyRollup(policy.Name, len(targets), atRisk)

	// Batch prefetch observability: "resolved" counts identities whose
	// BatchInputs entry actually got at least one CPU or memory sample back
	// -- deliberately NOT the same thing as "did not fail". A young workload
	// with no history yet resolves to zero samples on a perfectly healthy
	// Prometheus, same as it would during an outage; only batchStats.Failures
	// (recorded via EmitPolicyBatchFailures) tells the two apart. See
	// EmitPolicyBatchCoverage/EmitPolicyBatchFailures's doc comments for why
	// they are separate metrics.
	resolved := 0
	for _, c := range cands {
		if wi := batchInputs[c.Identity]; wi != nil && (len(wi.CPUPerPod) > 0 || len(wi.MemPerPod) > 0) {
			resolved++
		}
	}
	EmitPolicyBatchCoverage(policy.Name, len(cands), resolved)
	EmitPolicyBatchFailures(policy.Name, len(batchStats.Failures))

	// Sweep stale WorkloadRecommendations for this policy: any cached entry
	// whose target workload no longer exists (or is no longer matched by the
	// policy) is removed. Keeps etcd from accumulating dead cache entries.
	r.sweepWorkloadRecommendations(ctx, policy.Name, targets)

	// Counted against units, not len(targets) or len(items): failCount is
	// incremented once per dispatched unit of work, and neither of the other
	// two matches that. Departed identities are processed but never listed as
	// targets, and owner-name grouping puts several targets under one item.
	//
	// discoveryFailures is reported separately rather than folded into failed.
	// The two are different failures — one workload IDENTITY could not be REGISTERED for
	// computation, versus one that was processed and failed — and an identity
	// whose WLR already existed but whose status write failed would otherwise
	// be double counted. Surfacing it here is what stops a persistent
	// EnsureExists failure (missing RBAC, a rejecting admission webhook, a
	// quota) from reporting Ready while silently doing nothing: it flips the
	// condition to PartialFailure and increments reconcileTotal{result=error},
	// which is the countable signal an operator already alerts on.
	failed := int(failCount.Load())
	if failed > 0 || discoveryFailures > 0 {
		// The two clauses are independent, so each is emitted only when it has
		// something to say. "0 of 0 workloads failed; 3 identities could not be registered"
		// — the shape a discovery-only failure used to produce — buries the one
		// fact in the message under a clause that is both zero and misleading.
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
