package oomwatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

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

// minOwnerRetry is the soonest an owner chain that could not be read is read
// again.
const minOwnerRetry = 5 * time.Second

// Watcher reacts to Pod status changes and, on an OOM kill of a pod a Policy
// governs (governance.Decide), persists it into the Sink and optionally
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
	// Window is how long after a kill an owner chain that could not be read
	// is still retried: the recommender's LiveOOMWindow, past which a kill
	// adds nothing to what the recording rules counted. Zero means the
	// cache's default TTL.
	Window time.Duration

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
// annotation nor an OOM kill. Deletes are dropped too: an OOM kill never
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
		return len(oomKills(pod)) > 0
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return admits(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return admits(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return admits(e.Object) },
	}
}

// Reconcile fetches the pod, skips it fast if it shows no OOM kill, otherwise
// decides which Policy governs it and records its kills.
//
// Only an owner chain it could not read is requeued: otherwise the Pod
// informer fires again on the next status change (e.g. the next restart) and
// the Sink's dedup keeps replays cheap.
func (w *Watcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := w.Client.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	kills := oomKills(pod)
	if len(kills) == 0 {
		// Deliberately first, and before any API call: most pod transitions
		// are not OOM kills, so this keeps the owner/Namespace walk below off
		// the common path.
		return ctrl.Result{}, nil
	}

	// A kill stays in the pod's status for the rest of its life, so every
	// later status write re-triggers Reconcile. If every kill it shows was
	// already resolved on an earlier pass, the owner/Namespace walk has
	// nothing new to learn. This cannot reuse Sink.Record's dedup — that key
	// needs the resolved workload this walk would first have to produce.
	var unresolved []oomKill
	for _, k := range kills {
		if !w.Sink.AlreadyResolved(pod.UID, k.Container, k.RestartCount, k.TerminatedAt) {
			unresolved = append(unresolved, k)
		}
	}
	if len(unresolved) == 0 {
		return ctrl.Result{}, nil
	}

	reader := &podReader{client: w.Client, policies: &w.policies, excluded: w.ExcludedNamespaces}
	decision, err := governance.Decide(ctx, governance.Pod(pod), reader)
	if errors.Is(err, errOwnerUnresolved) {
		// Nothing is recorded and nothing marked resolved, so the walk is
		// retried, on the requeue or on the pod's next status event, whichever
		// comes first: a terminated Never pod may get no further event. The
		// pod's immediate controller is no substitute: a CronJob's Job or a
		// Deployment's ReplicaSet is an identity the inventory never holds,
		// so the kill would reach no recommendation and give
		// k8s_sustain_oom_observed_total a series per run.
		retry := w.ownerRetryAfter(unresolved)
		logger.V(1).Info("owner resolution failed; leaving the OOM kill unresolved", "err", err, "retryAfter", retry)
		return ctrl.Result{RequeueAfter: retry}, nil
	}
	if err != nil {
		// A failure reading the Policies or an annotation level is returned
		// rather than degraded to "absent": degrading would resolve to a less
		// specific level and silently un-manage the workload for as long as
		// the read keeps failing (RBAC gap, apiserver outage). Returning makes
		// controller-runtime requeue with backoff instead.
		return ctrl.Result{}, err
	}
	// Marked only now that every read this pass needed has succeeded: marking
	// a failed pass would suppress the walk for the whole cache TTL.
	for _, k := range kills {
		w.Sink.MarkResolved(pod.UID, k.Container, k.RestartCount, k.TerminatedAt)
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

	for _, k := range kills {
		key := Key{WorkloadIdentity: id, Container: k.Container}

		record := OOMRecord{
			Container:     k.Container,
			PolicyName:    policyName,
			ObservedAt:    now,
			TerminatedAt:  k.TerminatedAt,
			RestartCount:  k.RestartCount,
			PodName:       pod.Name,
			PodUID:        string(pod.UID),
			OOMLimitBytes: k.LimitBytes,
		}

		// Sink.Record is the single source of truth for dedup; fanning out on
		// anything but a genuinely new record turns a flapping pod into a
		// reconcile storm.
		if w.Sink.Record(key, record) {
			logger.V(1).Info(
				"OOM detected",
				"ownerKind", id.OwnerKind,
				"ownerName", id.OwnerName,
				"container", k.Container,
				"restarted", k.Restarted,
				"restartCount", k.RestartCount,
				"limitBytes", record.OOMLimitBytes,
			)
			if w.Handler != nil {
				w.Handler.OnOOMDetected(ctx, key, record)
			}
		}
	}

	return ctrl.Result{}, nil
}

// ownerRetryAfter is when to read again the owner chain of a pod showing
// kills: after as long as the newest of them has waited so far, so retries
// back off geometrically without counting attempts, and never once it is
// older than the window.
func (w *Watcher) ownerRetryAfter(kills []oomKill) time.Duration {
	var newest time.Time
	for _, k := range kills {
		if k.TerminatedAt.After(newest) {
			newest = k.TerminatedAt
		}
	}
	window := cmp.Or(w.Window, defaultTTL)
	age := w.now().Sub(newest)
	if age >= window {
		return 0
	}
	return min(max(age, minOwnerRetry), window-age)
}

// podReader is the governance.Reader of one Reconcile, reading straight
// through the manager's client.
type podReader struct {
	client   client.Client
	policies *governance.PolicyLister
	excluded []string
}

// errOwnerUnresolved wraps a failed read of a pod's controller chain (its
// ReplicaSet or Job).
var errOwnerUnresolved = errors.New("pod owner unresolved")

func (r *podReader) Policies(ctx context.Context) (*governance.Policies, error) {
	return r.policies.List(ctx, r.client, r.excluded)
}

func (r *podReader) PodOwner(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	kind, name, err := workload.ResolvePodOwner(ctx, r.client, pod)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errOwnerUnresolved, err)
	}
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

// oomKill is one OOM kill a pod's status shows. Container, RestartCount and
// TerminatedAt identify it without the resolved workload, which is what
// Sink.AlreadyResolved and MarkResolved key on.
type oomKill struct {
	Container string
	// Restarted says the container restarted after the kill; false for a
	// container that will not restart.
	Restarted    bool
	RestartCount int32
	TerminatedAt time.Time
	LimitBytes   int64
}

// oomKills lists the OOM kills pod's containers show, by the definition the
// k8s_sustain:container_oom_kills recording rule counts (docs/adr/0010): a
// last termination that is OOMKilled, which the container restarted after,
// or, in a pod whose restartPolicy is Never, a current OOMKilled termination.
// Under Always or OnFailure a current termination is the brief state before
// the restart, which counts the kill instead.
func oomKills(pod *corev1.Pod) []oomKill {
	never := pod.Spec.RestartPolicy == corev1.RestartPolicyNever
	var out []oomKill
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		term, restarted := cs.LastTerminationState.Terminated, true
		if !isOOMKill(term) && never {
			term, restarted = cs.State.Terminated, false
		}
		if !isOOMKill(term) {
			continue
		}
		out = append(out, oomKill{
			Container:    cs.Name,
			Restarted:    restarted,
			RestartCount: cs.RestartCount,
			TerminatedAt: term.FinishedAt.Time,
			LimitBytes:   containerMemLimitBytes(pod, cs.Name),
		})
	}
	return out
}

func isOOMKill(term *corev1.ContainerStateTerminated) bool {
	return term != nil && term.Reason == oomKilledReason
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
