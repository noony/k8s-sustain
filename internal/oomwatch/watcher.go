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

// oomKilledReason is the literal Kubelet writes into
// ContainerStateTerminated.Reason on a kernel OOM kill. Matched exactly: it is
// a documented CRI-status contract that kubectl's own UX also depends on.
const oomKilledReason = "OOMKilled"

// maxConcurrentReconciles keeps the API client and Sink from being hammered
// when a whole Deployment OOMs at once, while still draining fast enough for
// the "as it happens" SLO.
const maxConcurrentReconciles = 4

// Watcher reacts to Pod status changes and, on a fresh OOMKill of a pod
// managed by a Policy (resolved across all three annotation levels — see
// policymatch.ResolvePolicy), persists it into the Sink and optionally
// notifies an EventHandler so the Policy reconciler re-triggers immediately.
//
// It owns no state: dedup and TTLs belong to the Sink, fan-out to the Handler.
type Watcher struct {
	Client  client.Client
	Sink    Sink
	Handler EventHandler // optional; may be nil
	// Now is injectable so tests get stable ObservedAt values. Defaults to
	// time.Now when nil.
	Now func() time.Time
}

// SetupWithManager registers the Watcher as a Pod controller. The predicate is
// load-bearing: without it the watcher wakes on every pod transition in the
// cluster, which is not free even when each reconcile is a no-op.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("oomwatch").
		For(&corev1.Pod{}).
		WithEventFilter(admitPodEventPredicate()).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(w)
}

// admitPodEventPredicate drops pod events carrying neither the policy
// annotation nor a fresh OOMKill. Deletes are dropped too: an OOM kill never
// surfaces as a Pod delete, and chasing a deleted pod only races the GC.
//
// The OOM-status arm is required because pods inherit only the pod-template
// annotation level: a workload opted in at the workload or Namespace level
// produces unannotated pods, so the annotation check alone would never admit
// their OOM events. It is the only local (no apiserver call) signal that such
// an event may still be worth Reconcile's owner/Namespace walk.
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
		// Deliberately first, and before any API call: most pod transitions
		// are not OOM kills, so this keeps the owner/Namespace walk below off
		// the common path.
		return ctrl.Result{}, nil
	}

	// LastTerminationState stays OOMKilled for the rest of the pod's life, so
	// a chronically OOM-restarting pod re-triggers Reconcile on every status
	// write. If every termination it reports was already resolved on an
	// earlier pass, the owner/Namespace walk has nothing new to learn. This
	// cannot reuse Sink.Record's dedup — that key needs the resolved workload
	// this walk would first have to produce.
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

	// The owner and Namespace reads are lazy: each is issued only when no more
	// specific level already decided (policymatch.DecidesAt), so a
	// pod-template-only workload never pays for a Get it does not need.
	//
	// Unlike the owner-resolution fallback above, a non-NotFound read failure
	// is returned rather than degraded to "absent": degrading would resolve to
	// a less specific level and silently un-manage the workload for as long as
	// the read keeps failing (RBAC gap, apiserver outage). Returning makes
	// controller-runtime requeue with backoff instead. Same rule as
	// nsAnnotations.get in internal/controller/namespace_annotations.go.
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
	// Every read this pass needed has now succeeded, so this is the place to
	// mark — except on a degraded owner resolution. That path does not return
	// early, and when the pod template alone decides the policy the failed
	// ResolvePodOwner is never retried here. Marking anyway would suppress the
	// walk for the whole cache TTL, permanently pinning the OOM under the
	// degraded bucket (e.g. ReplicaSet, which RecentByWorkload never queries)
	// instead of self-healing to the real owner.
	if !degradedOwner {
		for _, t := range terms {
			w.Sink.MarkResolved(pod.UID, t.Container, t.RestartCount, t.TerminatedAt)
		}
	}

	if policyName == "" {
		// Load-bearing on informer resync: the predicate only filters event
		// delivery, but Reconcile also runs for cached objects on cold start
		// and after watch-cache restarts, where a de-annotated pod leaks
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

		// Sink.Record is the single source of truth for dedup; fanning out on
		// anything but a genuinely new record turns a flapping pod into a
		// reconcile storm.
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

// ownerAnnotations reads the owning workload's own metadata.annotations — the
// level a pod does not inherit. An unknown kind, an empty kind, and a missing
// object all resolve to nil rather than an error; none is a failure.
//
// It goes through the shared workload.ObjectForKind table so this and
// internal/webhook's namesake cannot diverge on which kinds they can Get.
//
// A Get here is only free when the manager already caches that kind, which it
// does only for kinds some Policy enables (collectTargets skips the rest), and
// never for ReplicaSet. So an OOM on, say, a CronJob pod can stand up a
// cluster-wide informer for one object. Those kinds are few, so the cost is
// small — but it is not the blanket "no new informer" guarantee it looks like.
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

// oomTerminations returns one containerTermination per container reporting a
// fresh OOMKilled LastTerminationState — the same signal hasFreshOOM checks,
// but carrying the identity Reconcile's pre-walk dedup needs.
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

// containerMemLimitBytes returns the memory limit (bytes) the kernel actually
// killed the container at, or zero if none is set.
//
// ContainerStatus.Resources (what the kubelet applied) wins over pod.Spec
// (what we asked for). Anchoring the OOM floor on the spec feeds the floor its
// own output — limit * 1.20 bump * 1.15 headroom, limit = 1.50 * request, so
// ~2.07x per kill with nothing to converge on. The two diverge while a resize
// is pending and permanently once one is infeasible.
//
// pod.Spec is the fallback: Status.Resources is only populated on in-place
// resize-capable pods (k8s >= 1.33), which have no divergence to worry about.
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
