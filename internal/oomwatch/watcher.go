package oomwatch

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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
// policy-annotated pod, persists it into the Sink and (optionally) notifies an
// EventHandler so the Policy reconciler can be re-triggered immediately.
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
// that filters out pods without the policy annotation up front; without that
// pre-filter the watcher would wake up on every pod transition in the cluster,
// which is not free even when each reconcile is a no-op.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("oomwatch").
		For(&corev1.Pod{}).
		WithEventFilter(hasPolicyAnnotationPredicate()).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(w)
}

// hasPolicyAnnotationPredicate drops any pod event whose object does not carry
// the policy annotation. Delete events are dropped too: an OOM kill never
// surfaces as a Pod delete (the pod restarts in place), and chasing a deleted
// pod would only race with the GC.
func hasPolicyAnnotationPredicate() predicate.Predicate {
	hasAnnotation := func(obj client.Object) bool {
		if obj == nil {
			return false
		}
		_, ok := obj.GetAnnotations()[sustainv1alpha1.PolicyAnnotation]
		return ok
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return hasAnnotation(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return hasAnnotation(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return hasAnnotation(e.Object) },
	}
}

// Reconcile fetches the pod and scans its container statuses for fresh
// OOMKills. We always return a zero Result: nothing here benefits from
// requeue, because the Pod informer will fire again on the next status change
// (e.g. the next restart) and the Sink's dedup keeps replays cheap.
func (w *Watcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	if err := w.Client.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	policyName, ok := pod.GetAnnotations()[sustainv1alpha1.PolicyAnnotation]
	if !ok {
		// Load-bearing on informer resync: the predicate only filters event
		// delivery, but Reconcile is called for cached objects on cold start
		// and after watch-cache restarts. A pod whose annotation was removed
		// without an Update event would otherwise leak through.
		return ctrl.Result{}, nil
	}

	if !hasFreshOOM(pod) {
		// Hot-path skip: most pod transitions (readiness flips, IP changes)
		// are not OOM kills. Returning here avoids the owner-walk API call.
		return ctrl.Result{}, nil
	}

	ownerKind, ownerName, err := workload.ResolvePodOwner(ctx, w.Client, pod)
	if err != nil {
		// Fall back to the immediate controller ref so a transient RS/Job
		// Get failure does not silently drop the OOM. Bucketing under the
		// intermediate kind is strictly better than losing the signal.
		ownerKind, ownerName = immediateController(pod)
		logger.V(1).Info("owner resolution failed; falling back to immediate controller",
			"kind", ownerKind, "name", ownerName, "err", err)
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
			logger.V(1).Info("OOM detected",
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

// containerMemLimitBytes returns the configured memory limit (in bytes) for
// the named container from the pod spec, or zero if no limit is set. We read
// the spec rather than the status because Status.Resources only exists for
// in-place resize-capable pods (k8s >= 1.27 and feature-gated until 1.33).
func containerMemLimitBytes(pod *corev1.Pod, container string) int64 {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != container {
			continue
		}
		if c.Resources.Limits == nil {
			return 0
		}
		q := c.Resources.Limits.Memory()
		if q == nil || q.IsZero() {
			return 0
		}
		v, ok := q.AsInt64()
		if !ok {
			return 0
		}
		return v
	}
	return 0
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
