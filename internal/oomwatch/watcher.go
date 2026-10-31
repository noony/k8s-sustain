package oomwatch

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
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

// Watcher reacts to Pod status changes and, on a fresh OOMKill of a pod a
// Policy governs (governance.Decide), persists it into the Sink and optionally
// notifies an EventHandler so the governing Policy's reconciler re-triggers
// immediately.
//
// It owns no state: dedup and TTLs belong to the Sink, fan-out to the Handler.
type Watcher struct {
	Client  client.Client
	Sink    Sink
	Handler EventHandler // optional; may be nil
	// ExcludedNamespaces mirrors the controller's --excluded-namespaces: no
	// Policy governs a pod there.
	ExcludedNamespaces []string
	// Now is injectable so tests get stable ObservedAt values. Defaults to
	// time.Now when nil.
	Now func() time.Time

	policies governance.PolicyLister
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
// otherwise decides which Policy governs it and scans its container statuses
// for the kill to record.
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

	// Unlike the owner fallback (podReader.PodOwner), a failure reading the
	// Policies or an annotation level is returned rather than degraded to
	// "absent": degrading would resolve to a less specific level and silently
	// un-manage the workload for as long as the read keeps failing (RBAC gap,
	// apiserver outage). Returning makes controller-runtime requeue with
	// backoff instead.
	reader := &podReader{client: w.Client, policies: &w.policies, excluded: w.ExcludedNamespaces, logger: logger}
	decision, err := governance.Decide(ctx, governance.Pod(pod), reader)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Every read this pass needed has now succeeded, so this is the place to
	// mark — except on a degraded owner resolution, which no later read of
	// this pass retries. Marking anyway would suppress the walk for the whole
	// cache TTL, permanently pinning the OOM under the degraded owner (e.g. a
	// ReplicaSet, which no Policy governs) instead of self-healing to the real
	// one.
	if !reader.degraded {
		for _, t := range terms {
			w.Sink.MarkResolved(pod.UID, t.Container, t.RestartCount, t.TerminatedAt)
		}
	}

	if !decision.Governed() {
		// Load-bearing on informer resync: the predicate only filters event
		// delivery, but Reconcile also runs for cached objects on cold start
		// and after watch-cache restarts, where a de-annotated pod leaks
		// through.
		return ctrl.Result{}, nil
	}
	// The identity is the one the controller and the webhook key the
	// WorkloadRecommendation and its Prometheus series by; a live OOM cached
	// under any other is never found by ByWorkload.
	id, policyName := decision.Identity, decision.PolicyName()

	now := w.now()

	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		term := cs.LastTerminationState.Terminated
		if term == nil || term.Reason != oomKilledReason {
			continue
		}

		key := Key{WorkloadIdentity: id, Container: cs.Name}

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
				"ownerKind", id.OwnerKind,
				"ownerName", id.OwnerName,
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

// podReader is the governance.Reader of one Reconcile, reading straight
// through the manager's client.
type podReader struct {
	client   client.Client
	policies *governance.PolicyLister
	excluded []string
	logger   logr.Logger
	// degraded records that the owner fell back to the pod's immediate
	// controller.
	degraded bool
}

func (r *podReader) Policies(ctx context.Context) (*governance.Policies, error) {
	return r.policies.List(ctx, r.client, r.excluded)
}

// PodOwner falls back to the immediate controller ref so a transient RS/Job
// Get failure does not fail the pass: a Job's OOM is still recorded under the
// Job when its Policy manages Jobs. Under a kind no Policy manages (a
// ReplicaSet) nothing is recorded, and since a degraded pass marks nothing
// resolved, the pod's next status event retries the walk.
func (r *podReader) PodOwner(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	kind, name, err := workload.ResolvePodOwner(ctx, r.client, pod)
	if err == nil {
		return kind, name, nil
	}
	r.degraded = true
	kind, name = immediateController(pod)
	r.logger.V(1).Info("owner resolution failed; falling back to immediate controller",
		"kind", kind, "name", name, "err", err)
	return kind, name, nil
}

// WorkloadAnnotations reads the owning workload's own metadata.annotations —
// the level a pod does not inherit.
//
// A Get here is only free when the manager already caches that kind, which it
// does only for kinds some Policy manages (inventory.TakeForPolicy lists no
// other), and never for ReplicaSet. So an OOM on, say, a CronJob pod can stand
// up a cluster-wide informer for one object. Those kinds are few, so the cost
// is small — but it is not the blanket "no new informer" guarantee it looks
// like.
func (r *podReader) WorkloadAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	return workload.OwnerAnnotations(ctx, r.client, namespace, kind, name)
}

func (r *podReader) NamespaceAnnotations(ctx context.Context, namespace string) (map[string]string, error) {
	return workload.NamespaceAnnotations(ctx, r.client, namespace)
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
