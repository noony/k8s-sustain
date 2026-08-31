package oomwatch

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// oomKilledReason is the literal string Kubelet writes into
// ContainerStateTerminated.Reason when the kernel OOM-killer takes a process.
// We match it exactly because it's a documented contract surfaced by the CRI
// status translator; any change there would also break the kubectl UX.
const oomKilledReason = "OOMKilled"

// maxConcurrentReconciles caps the number of in-flight pod reconciles. Pod
// status events can come in bursts (e.g. when a whole Deployment OOMs on
// rollout); a small bound keeps the API client and Sink from being hammered
// while still draining the work queue quickly enough that we don't miss the
// "as it happens" SLO.
const maxConcurrentReconciles = 4

// Watcher reacts to Pod status changes and, when it spots a fresh OOMKill on a
// pod managed by a Policy (resolved across all three annotation levels — see
// policymatch.ResolvePolicy), persists it into the Sink and (optionally)
// notifies an EventHandler so the Policy reconciler can be re-triggered
// immediately.
//
// The Watcher itself owns no state: dedup, TTLs, and downstream fan-out are
// the Sink's and Handler's responsibilities respectively. That keeps this type
// trivially unit-testable against a fake client.
type Watcher struct {
	Client  client.Client
	Sink    Sink
	Handler EventHandler // optional; may be nil
	// Now is injectable so tests get stable ObservedAt values. Defaults to
	// time.Now when nil.
	Now func() time.Time
}

// SetupWithManager registers the Watcher as a Pod controller with a predicate
// that filters out pods with neither the policy annotation nor a fresh OOMKill
// up front; without that pre-filter the watcher would wake up on every pod
// transition in the cluster, which is not free even when each reconcile is a
// no-op.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("oomwatch").
		For(&corev1.Pod{}).
		WithEventFilter(admitPodEventPredicate()).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(w)
}

// admitPodEventPredicate drops any pod event whose object carries
// neither the policy annotation nor a fresh OOMKill. Delete events are
// dropped too: an OOM kill never surfaces as a Pod delete (the pod restarts
// in place), and chasing a deleted pod would only race with the GC.
//
// The OOM-status arm exists because Reconcile now resolves the policy from
// three levels (pod template, owning workload, Namespace — see
// policymatch.ResolvePolicy), and pods only ever inherit the pod-template
// level. A workload opted in at the workload or Namespace level produces pods
// with no annotation of their own, so the annotation check alone would never
// admit their OOM events — exactly the bug this predicate exists to not
// reintroduce. The OOM-status check is the only local signal (no apiserver
// call, same as the annotation check) that such a pod's event might still be
// worth Reconcile's owner/Namespace walk; Reconcile itself does the real
// resolution and drops anything that turns out not to be managed.
func admitPodEventPredicate() predicate.Predicate {
	admits := func(obj client.Object) bool {
		if obj == nil {
			return false
		}
		if _, ok := obj.GetAnnotations()[sustainv1alpha1.PolicyAnnotation]; ok {
			return true
		}
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return false
		}
		return hasFreshOOM(pod)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return admits(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return admits(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return admits(e.Object) },
	}
}

// Reconcile fetches the pod, skips it fast if it carries no fresh OOMKill,
// otherwise resolves the Policy it opts into across all three annotation
// levels and scans its container statuses for the kill to record.
//
// We always return a zero Result: nothing here benefits from requeue, because
// the Pod informer will fire again on the next status change (e.g. the next
// restart) and the Sink's dedup keeps replays cheap.
func (w *Watcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := w.Client.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	terms := oomTerminations(pod)
	if len(terms) == 0 {
		// Hot-path skip #1, and deliberately FIRST: most pod transitions
		// (readiness flips, IP changes) are not OOM kills, and this is what
		// keeps the owner/Namespace walk below off the common path. Nothing
		// before this line makes an API call.
		return ctrl.Result{}, nil
	}

	// Hot-path skip #2: LastTerminationState.Terminated.Reason stays
	// OOMKilled for the rest of the pod's life (see admitPodEventPredicate's
	// doc), so a chronically OOM-restarting pod re-triggers Reconcile on
	// EVERY status write, not just the transition — readiness flips, IP
	// changes, anything. Once every fresh OOM this pod currently reports has
	// already been resolved on an earlier pass (same RestartCount +
	// TerminatedAt per container — a genuinely new kill bumps at least one
	// of those), there is nothing new to learn from the owner/Namespace walk
	// below, whether or not this pod turned out to be managed by any
	// Policy — skip it. See Sink.AlreadyResolved's doc for why this check
	// cannot reuse Sink.Record's own dedup (that one is keyed by the
	// resolved workload, which is exactly what this walk would still have to
	// produce).
	allResolved := true
	for _, t := range terms {
		if !w.Sink.AlreadyResolved(pod.UID, t.Container, t.RestartCount, t.TerminatedAt) {
			allResolved = false
			break
		}
	}
	if allResolved {
		return ctrl.Result{}, nil
	}

	ownerKind, ownerName, err := workload.ResolvePodOwner(ctx, w.Client, pod)
	// degradedOwner tracks whether the line above fell back to the immediate
	// controller ref instead of the fully-resolved owner. It gates the
	// MarkResolved call below — see that comment for why.
	degradedOwner := err != nil
	if degradedOwner {
		// Fall back to the immediate controller ref so a transient RS/Job
		// Get failure does not silently drop the OOM. Bucketing under the
		// intermediate kind is strictly better than losing the signal.
		ownerKind, ownerName = immediateController(pod)
		logger.V(1).Info("owner resolution failed; falling back to immediate controller",
			"kind", ownerKind, "name", ownerName, "err", err)
	}

	// Resolve the Policy this pod opts into across all three annotation
	// levels — pod template, owning workload, Namespace — because pods only
	// ever inherit the pod-template level (see policymatch.ResolvePolicy).
	//
	// The owner and Namespace reads are LAZY: each is only issued when no
	// more specific level already decided the outcome (policymatch.DecidesAt).
	// The pod template is free (already in hand from the Get above), so
	// checking it first costs nothing. Resolving all three unconditionally,
	// as an earlier version of this function did, coupled every existing
	// pod-template-only workload to an owner Get and a Namespace read it
	// never needed.
	//
	// Unlike the owner-resolution fallback above, a non-NotFound read failure
	// on either of these IS returned rather than degraded to "absent":
	// degrading would resolve to a less specific level than the one that
	// actually failed, silently un-managing the workload for as long as the
	// read keeps failing (RBAC gap, apiserver outage), with no signal beyond
	// an off-by-default V(1) log line. Returning the error instead makes
	// controller-runtime requeue with backoff, so a transient failure is
	// retried rather than swallowed. This mirrors the decision made for the
	// identical Namespace read in
	// internal/controller/namespace_annotations.go (nsAnnotations.get): a
	// missing object is not an error, but a genuinely broken read must
	// surface. NotFound still resolves to nil annotations, not an error — a
	// namespace or owner object can legitimately vanish mid-reconcile.
	var policyName string
	podAnn := pod.GetAnnotations()
	switch {
	case policymatch.DecidesAt(podAnn):
		policyName, _ = policymatch.ResolvePolicy(podAnn, nil, nil)
	default:
		ownerAnn, err := w.ownerAnnotations(ctx, pod.Namespace, ownerKind, ownerName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if policymatch.DecidesAt(ownerAnn) {
			policyName, _ = policymatch.ResolvePolicy(podAnn, ownerAnn, nil)
		} else {
			nsAnn, err := w.namespaceAnnotations(ctx, pod.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}
			policyName, _ = policymatch.ResolvePolicy(podAnn, ownerAnn, nsAnn)
		}
	}
	// Every owner/Namespace Get this pass still needed has now happened and
	// succeeded (both the ownerAnnotations and namespaceAnnotations error
	// branches above return before reaching here), so this is the right
	// place to mark — EXCEPT when ResolvePodOwner itself degraded to the
	// immediate controller ref. That degrade does not return early: it is a
	// deliberate "bucket under the intermediate kind rather than lose the
	// signal" fallback, so the switch above still runs to completion and
	// reaches this line. But when the pod template alone decides the policy
	// (the common opt-in path), ownerAnnotations is never even called on
	// this pass, so the ResolvePodOwner failure that produced ownerKind is
	// never retried here. Marking anyway would let AlreadyResolved suppress
	// the owner/Namespace walk on every later status write for this
	// termination — up to the cache TTL — even after the apiserver read
	// that failed has recovered, permanently pinning the OOM under the
	// degraded (e.g. ReplicaSet, which RecentByWorkload never queries)
	// bucket instead of self-healing to the real owner. So: skip the mark
	// on a degraded resolution and let the next status event (readiness
	// flip, IP change, ...) retry the walk from scratch.
	if !degradedOwner {
		for _, t := range terms {
			w.Sink.MarkResolved(pod.UID, t.Container, t.RestartCount, t.TerminatedAt)
		}
	}

	if policyName == "" {
		// Load-bearing on informer resync: the predicate only filters event
		// delivery, but Reconcile is called for cached objects on cold start
		// and after watch-cache restarts. A pod that resolves to no policy at
		// any level — e.g. its opt-in annotation was removed at any level
		// without an Update event reaching this pod — would otherwise leak
		// through.
		return ctrl.Result{}, nil
	}

	// Keep the live-OOM cache key consistent with the identity the
	// controller/webhook query Prometheus and the WorkloadRecommendation
	// under — otherwise an overridden identity's live OOM signal is cached
	// under the real owner and never found by RecentByWorkload lookups.
	ownerKind, ownerName = workload.ApplyOwnerNameOverride(ownerKind, ownerName, pod.GetAnnotations())
	if ownerKind == "" {
		return ctrl.Result{}, nil
	}

	now := w.now()

	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		term := cs.LastTerminationState.Terminated
		if term == nil || term.Reason != oomKilledReason {
			continue
		}

		key := Key{
			Namespace: pod.Namespace,
			OwnerKind: ownerKind,
			OwnerName: ownerName,
			Container: cs.Name,
		}

		record := OOMRecord{
			Container:     cs.Name,
			PolicyName:    policyName,
			ObservedAt:    now,
			TerminatedAt:  term.FinishedAt.Time,
			RestartCount:  cs.RestartCount,
			PodName:       pod.Name,
			PodUID:        string(pod.UID),
			OOMLimitBytes: containerMemLimitBytes(pod, cs.Name),
		}

		// Sink.Record is the single source of truth for dedup. Only fan out
		// to the handler when the record is genuinely new, otherwise a flap-
		// ping pod would amplify into a reconcile storm.
		if w.Sink.Record(key, record) {
			logger.V(1).Info(
				"OOM detected",
				"ownerKind", ownerKind,
				"ownerName", ownerName,
				"container", cs.Name,
				"restartCount", cs.RestartCount,
				"limitBytes", record.OOMLimitBytes,
			)
			if w.Handler != nil {
				w.Handler.OnOOMDetected(ctx, key, record)
			}
		}
	}

	return ctrl.Result{}, nil
}

// ownerAnnotations reads the owning workload object's own
// metadata.annotations — the level a pod does not inherit. An unknown kind
// (a custom controller), an empty kind (an orphan pod), and a missing object
// all resolve to nil rather than an error: none of those is a failure, each
// simply means there is no workload level to read.
//
// This mirrors internal/webhook's ownerAnnotations of the same name; both
// read through the shared workload.ObjectForKind table so the two readers
// cannot silently diverge on which kinds they know how to Get. Unlike the
// webhook, the controller manager's cache already caches most of the kinds in
// that table for "no new informer" free — but only the ones some Policy
// actually enables. collectTargets (internal/controller/target_listing.go)
// skips listing any kind whose UpdateMode is nil, so on a cluster whose
// Policies enable only, say, deployment, the manager never lists
// StatefulSets, DaemonSets, CronJobs, Jobs, or Rollouts either — and Getting
// one of those here is then the process's first Get of that kind, standing up
// a cluster-wide informer for the sake of one object. CronJob is the likeliest
// case in practice: ResolvePodOwner never Gets a CronJob itself (it reads the
// Job's ownerRef), so an OOM on a CronJob pod can be the very first CronJob
// Get the controller ever makes. ReplicaSet is never manager-cached at all,
// regardless of which kinds a Policy enables: collectTargets has no
// "listReplicaSetTargets" entry to skip in the first place (Deployment
// workloads are listed and reconciled directly), so any Get of a ReplicaSet
// here always stands up a fresh informer — whether ownerKind == "ReplicaSet"
// arrived via a genuine orphan ReplicaSet (one with no Deployment/Rollout
// owner, ResolvePodOwner's own terminal case) or via the immediateController
// fallback when ResolvePodOwner's Get failed. These kinds are typically few,
// so the impact is small, but it is not the "no new informer" guarantee this
// comment used to claim held for every kind except Rollout — the real rule is
// "cached only if some Policy enables it" (and ReplicaSet is never cached by
// that rule at all), which can affect more than just Rollout.
func (w *Watcher) ownerAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	obj := workload.ObjectForKind(kind)
	if obj == nil {
		return nil, nil
	}
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj.GetAnnotations(), nil
}

// namespaceAnnotations reads the Namespace object's own metadata.annotations
// — the least specific opt-in level. A missing Namespace resolves to nil
// rather than an error: a pod being reconciled in a namespace the apiserver
// no longer has is a deletion race, not a failure worth surfacing.
func (w *Watcher) namespaceAnnotations(ctx context.Context, name string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := w.Client.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	return ns.Annotations, nil
}

func immediateController(pod *corev1.Pod) (kind, name string) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind, ref.Name
		}
	}
	return "", ""
}

func hasFreshOOM(pod *corev1.Pod) bool {
	for i := range pod.Status.ContainerStatuses {
		term := pod.Status.ContainerStatuses[i].LastTerminationState.Terminated
		if term != nil && term.Reason == oomKilledReason {
			return true
		}
	}
	return false
}

// containerTermination is the identity oomTerminations extracts for one
// container's fresh OOMKilled LastTerminationState: enough to key
// Sink.AlreadyResolved/MarkResolved without needing the resolved workload.
type containerTermination struct {
	Container    string
	RestartCount int32
	TerminatedAt time.Time
}

// oomTerminations returns one containerTermination per container currently
// reporting a fresh OOMKilled LastTerminationState — the same signal
// hasFreshOOM checks for (len(oomTerminations(pod)) > 0 iff hasFreshOOM(pod)),
// but carrying enough identity (RestartCount, TerminatedAt) for Reconcile's
// pre-walk dedup check.
func oomTerminations(pod *corev1.Pod) []containerTermination {
	var out []containerTermination
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		term := cs.LastTerminationState.Terminated
		if term == nil || term.Reason != oomKilledReason {
			continue
		}
		out = append(out, containerTermination{
			Container:    cs.Name,
			RestartCount: cs.RestartCount,
			TerminatedAt: term.FinishedAt.Time,
		})
	}
	return out
}

// containerMemLimitBytes returns the memory limit (in bytes) the kernel
// actually killed the named container at, or zero if no limit is set.
//
// ContainerStatus.Resources — the limits the kubelet successfully applied — is
// the authoritative source and is preferred over pod.Spec. pod.Spec carries
// the DESIRED limit, which the recommender itself rewrites on every in-place
// resize, so anchoring the OOM memory floor there feeds the floor its own
// previous output: floor = limit * 1.20 (bump) * 1.15 (headroom), new limit =
// 1.50 * request, hence ~2.07x per kill with nothing to converge on. The two
// diverge for as long as a resize is pending, and permanently once a resize is
// infeasible (a request above node capacity is never applied, so the container
// keeps dying at its old small limit while the spec grows without bound).
//
// pod.Spec remains the fallback: Status.Resources is only populated for
// in-place resize-capable pods (k8s >= 1.33), and a pod that has never been
// resized has no divergence to worry about.
func containerMemLimitBytes(pod *corev1.Pod, container string) int64 {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != container || cs.Resources == nil {
			continue
		}
		// Applied state is authoritative once present, including when it
		// carries no memory limit at all — a resize that removed the limit
		// must not fall through and resurrect the stale spec value.
		return memLimitBytes(cs.Resources.Limits)
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != container {
			continue
		}
		return memLimitBytes(c.Resources.Limits)
	}
	return 0
}

// memLimitBytes extracts a memory limit in bytes from a ResourceList,
// returning zero when no usable limit is present.
func memLimitBytes(limits corev1.ResourceList) int64 {
	if limits == nil {
		return 0
	}
	q := limits.Memory()
	if q == nil || q.IsZero() {
		return 0
	}
	v, ok := q.AsInt64()
	if !ok {
		return 0
	}
	return v
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
