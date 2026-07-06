package workload

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ContainerRecommendation holds computed resource changes for a single container.
type ContainerRecommendation struct {
	CPURequest        *resource.Quantity
	CPULimit          *resource.Quantity
	RemoveCPULimit    bool
	MemoryRequest     *resource.Quantity
	MemoryLimit       *resource.Quantity
	RemoveMemoryLimit bool
}

// SafeToEvictAnnotation is the cluster-autoscaler annotation that marks a
// pod as not safe to evict. Matching cluster-autoscaler's own convention,
// only the literal value "false" blocks eviction; absence or any other
// value has no effect. In-place resizes are never gated by it — the
// annotation is about disruption, and a resize does not disrupt the pod.
const SafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// RecycleOption configures a RecyclePods / ResizePodsInPlace call.
type RecycleOption func(*recycleOptions)

type recycleOptions struct {
	tol               Tolerance
	observe           func(resource string)
	ignoreSafeToEvict bool
}

func newRecycleOptions(opts []RecycleOption) recycleOptions {
	var o recycleOptions
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithTolerance suppresses pod recycling for resource decreases below the given
// per-resource bands. The zero Tolerance (the default when this option is
// omitted) disables suppression entirely.
func WithTolerance(tol Tolerance) RecycleOption {
	return func(o *recycleOptions) { o.tol = tol }
}

// WithSuppressionObserver registers a callback invoked once per resource
// ("cpu"/"memory") for each pod whose decrease in that resource was suppressed
// by the tolerance. Used by the controller to emit a metric.
func WithSuppressionObserver(fn func(resource string)) RecycleOption {
	return func(o *recycleOptions) { o.observe = fn }
}

// WithIgnoreSafeToEvictAnnotations disables the safe-to-evict gate: pods
// annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` are
// evicted like any other pod. The default (option omitted or false) never
// evicts such pods; they are skipped like a PDB-blocked eviction and
// re-considered on the next reconcile.
func WithIgnoreSafeToEvictAnnotations(ignore bool) RecycleOption {
	return func(o *recycleOptions) { o.ignoreSafeToEvict = ignore }
}

// podContainers returns a pod's regular + init containers as one slice, for
// per-pod tolerance evaluation.
func podContainers(pod *corev1.Pod) []corev1.Container {
	out := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	out = append(out, pod.Spec.Containers...)
	out = append(out, pod.Spec.InitContainers...)
	return out
}

// Patcher recycles pods of Kubernetes workloads so they pick up the latest
// resource recommendations (injected by the admission webhook at pod creation).
//
// The Patcher never modifies workload specs (Deployment, StatefulSet, etc.).
//
// Ongoing mode behaviour:
//   - k8s ≥ 1.33 (inPlace=true): patches running pods directly via
//     InPlacePodVerticalScaling — zero restarts. Uses the /resize subresource.
//   - k8s < 1.33 (inPlace=false): evicts stale pods one by one via the
//     Eviction API so the workload controller replaces them. The webhook
//     injects the latest resources into the replacement pods.
//     PodDisruptionBudgets are respected; pods blocked by a PDB are
//     skipped and retried on the next reconcile cycle.
//     After each eviction the patcher waits for the replacement pod to
//     become Ready before evicting the next one, and aborts the loop if
//     any pod in the selector enters CrashLoopBackOff (likely caused by
//     a bad recommendation).
//     For StatefulSets, pods are processed in descending ordinal order to
//     respect the StatefulSet's update semantics.
type Patcher struct {
	client client.Client
	// inPlace is fixed at construction from the detected server version.
	inPlace bool

	// readyPollInterval is how often we poll pod state while waiting for a
	// replacement pod to become Ready after an eviction.
	readyPollInterval time.Duration
	// readyTimeout caps how long we wait for a replacement pod. When it
	// fires we surface the failure so the caller can give up rather than
	// continuing to evict into a broken workload.
	readyTimeout time.Duration
}

// Option configures a Patcher at construction time.
type Option func(*Patcher)

// WithReadyPollInterval sets how often the patcher re-checks pod state
// while waiting for a replacement after an eviction. Tests use a tight
// interval; production keeps the default.
func WithReadyPollInterval(d time.Duration) Option {
	return func(p *Patcher) {
		if d > 0 {
			p.readyPollInterval = d
		}
	}
}

// WithReadyTimeout caps how long the patcher waits for a replacement pod
// to become Ready after evicting one. A long timeout protects fragile
// workloads from being hammered; a short timeout speeds tests up.
func WithReadyTimeout(d time.Duration) Option {
	return func(p *Patcher) {
		if d > 0 {
			p.readyTimeout = d
		}
	}
}

// Defaults for the eviction throttle. The replacement timeout has to cover
// the slowest realistic recovery path on the cluster — node provisioning via
// cluster-autoscaler or Karpenter, followed by image pull onto a fresh node
// and init-container execution, regularly runs 2-3 minutes and can hit 5+
// on slow registries. We default to 5 minutes so normal scale-up doesn't
// trigger a false-positive abort; operators on faster or slower clusters
// can override via Patcher options (and the controller's
// RecycleReplacementTimeout / --recycle-replacement-timeout flag).
const (
	defaultReadyPollInterval = 2 * time.Second
	defaultReadyTimeout      = 5 * time.Minute
)

// New returns a Patcher. Set inPlace=true when the cluster supports
// InPlacePodVerticalScaling and the pods/resize subresource (k8s ≥ 1.33).
func New(c client.Client, inPlace bool, opts ...Option) *Patcher {
	p := &Patcher{
		client:            c,
		inPlace:           inPlace,
		readyPollInterval: defaultReadyPollInterval,
		readyTimeout:      defaultReadyTimeout,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// InPlace reports whether the patcher uses in-place pod resource updates.
func (p *Patcher) InPlace() bool { return p.inPlace }

// TargetWorkload identifies the workload whose pods are being recycled.
// RecyclePods uses the UID to drop pods that merely match the label selector
// but are not owned by the workload (bare debug pods, or a second workload
// with an overlapping selector that did not opt in via the policy
// annotation). Kind and Name are used for logging only. An empty UID
// disables the ownership check.
type TargetWorkload struct {
	Kind string
	Name string
	UID  types.UID
}

// RecyclePods drives running pods matching the given selector toward the
// recommended resources, skipping pods whose controller ownerRef chain does
// not resolve to the target workload. This is the only public entry point
// for pod recycling.
func (p *Patcher) RecyclePods(ctx context.Context, target TargetWorkload, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation, opts ...RecycleOption) error {
	return p.recyclePods(ctx, target, namespace, selector, recs, newRecycleOptions(opts))
}

// ResizePodsInPlace resizes the given pods in place, never falling back to
// eviction. It is the entry point for short-lived pods (Job, CronJob runs)
// where eviction would kill the work in progress: if the cluster doesn't
// support InPlacePodVerticalScaling, the resize is silently skipped and the
// new resources land on the next pod creation via the webhook.
//
// Returns the number of pods whose resize the API server actually accepted,
// so callers can report accurately: pods that were skipped, already at
// target, or rejected (per-pod Invalid) are not counted.
//
// The caller is responsible for filtering out pods that should not be touched
// (e.g. pods owned by Completed/Failed Jobs).
func (p *Patcher) ResizePodsInPlace(ctx context.Context, pods []*corev1.Pod, recs map[string]ContainerRecommendation, opts ...RecycleOption) (int, error) {
	o := newRecycleOptions(opts)
	logger := log.FromContext(ctx)
	if !p.inPlace {
		logger.V(1).Info("in-place resize disabled on this cluster; deferring to next pod creation via webhook")
		return 0, nil
	}

	var errs []error
	resized, processed, skipped := 0, 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return resized, ctx.Err()
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping pod", "pod", pod.Name, "phase", pod.Status.Phase, "deleting", pod.DeletionTimestamp != nil)
			skipped++
			continue
		}
		// Clamp per-pod: a pod's containers may carry different current
		// allocations than their siblings (e.g. a partially-applied prior
		// resize), so the downsize threshold must be evaluated against this
		// pod's actual resources, not the workload template.
		podRecs := ClampRecsToTolerance(podContainers(pod), recs, o.tol)
		observeSuppressed(recs, podRecs, o.observe)
		applied, err := p.resizePodInPlaceNoEvict(ctx, pod, podRecs)
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		if applied {
			resized++
		}
		processed++
	}
	logger.Info("in-place resize pass complete", "processed", processed, "skipped", skipped, "resized", resized, "errors", len(errs))
	return resized, errors.Join(errs...)
}

// unapplyStrategy encodes what to do when an in-place resize can't be applied.
// Both hooks return (evicted, err): evicted=true means the pod was evicted and
// the caller must run the post-eviction throttle/wait.
//
//   - onUnsatisfiable runs when the pod spec already carries the target
//     resources but the kubelet reports the staged resize can't complete
//     (verdict Infeasible, or Error after a failed actuation) — the spec lies
//     about what is actually allocated, so a staleness check against the spec
//     would wrongly conclude the pod is fresh.
//   - onUnapplied runs when the API server rejected the resize patch as
//     invalid for this pod; the pod object has been reverted to the server's
//     spec by then.
//
// unsatisfiableLog and unappliedLog are the call-site-specific log lines
// emitted before the strategy runs, preserving each public function's exact
// logging.
type unapplyStrategy struct {
	unsatisfiableLog string
	unappliedLog     string
	onUnsatisfiable  func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error)
	onUnapplied      func(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error)
}

// resizePendingReason returns the kubelet's verdict on a staged in-place
// resize: corev1.PodReasonInfeasible, corev1.PodReasonDeferred,
// corev1.PodReasonError (the kubelet accepted the resize but failed while
// actuating it and does not retry on its own), any future reason the kubelet
// may report, or "" when no resize is pending (none requested, in progress,
// or already applied).
//
// The verdict is reported through the PodResizePending and
// PodResizeInProgress conditions (k8s ≥ 1.33). A pending verdict wins over an
// in-progress error: it always refers to the most recent desired state.
func resizePendingReason(pod *corev1.Pod) string {
	errored := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodResizePending && c.Status == corev1.ConditionTrue {
			return c.Reason
		}
		if c.Type == corev1.PodResizeInProgress && c.Status == corev1.ConditionTrue && c.Reason == corev1.PodReasonError {
			errored = true
		}
	}
	if errored {
		return corev1.PodReasonError
	}
	return ""
}

// resizePodInPlaceNoEvict mirrors patchPodInPlace but never evicts.
// Unsatisfiable resizes and API rejections are logged and skipped — the next
// pod creation will pick up the new resources via the webhook. Returns
// whether a resize was actually applied for this pod.
func (p *Patcher) resizePodInPlaceNoEvict(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error) {
	applied, _, err := p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unsatisfiableLog: "staged in-place resize cannot complete for short-lived pod, skipping (next run will pick up new resources)",
		unappliedLog:     "skipping short-lived pod (next run via webhook)",
		// New resources will land at next pod creation via the webhook —
		// don't evict short-lived pods.
		onUnsatisfiable: func(context.Context, *corev1.Pod, string) (bool, error) {
			return false, nil
		},
		onUnapplied: func(context.Context, *corev1.Pod, map[string]ContainerRecommendation) (bool, error) {
			return false, nil
		},
	})
	return applied, err
}

// resizePodInPlaceWith holds the in-place resize body shared by patchPodInPlace
// and resizePodInPlaceNoEvict. The two diverge only in what happens when a
// resize can't be applied, captured by the strategy: applyRecsFiltered, the
// resize-verdict handling, the regular-container submit, and the sidecar
// handling are all identical. Returns (applied, evicted, err): applied=true
// means the API server accepted at least one resize patch for this pod.
//
// The kubelet's verdict on a staged resize (Infeasible/Deferred/Error) is only
// consulted when the pod spec already carries the target resources: a verdict
// always refers to the resize currently staged in the spec, so when the
// recommendation has since changed, the new target is submitted and the
// kubelet re-evaluates it (e.g. a lowered request may fit where the old one
// did not). When the spec IS the unsatisfiable target, the node can't deliver
// it and the strategy decides between eviction and skipping — note the spec
// matches the recommendation in that case, so the decision must not be gated
// on spec staleness.
func (p *Patcher) resizePodInPlaceWith(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, strat unapplyStrategy) (applied, evicted bool, err error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	base := pod.DeepCopy()
	containers, regChanged := applyRecsFiltered(pod.Spec.Containers, recs, nil)
	initContainers, initChanged := applyRecsFiltered(pod.Spec.InitContainers, recs, isRestartableInitContainer)
	if !regChanged && !initChanged {
		switch verdict := resizePendingReason(pod); verdict {
		case corev1.PodReasonInfeasible, corev1.PodReasonError:
			// Infeasible: the node can't fit the target. Error: the kubelet
			// failed actuating it and won't retry on its own. Either way the
			// staged resize is going nowhere without intervention.
			logger.Info(strat.unsatisfiableLog, "verdict", verdict)
			evicted, err = strat.onUnsatisfiable(ctx, pod, verdict)
			return false, evicted, err
		case corev1.PodReasonDeferred:
			logger.Info("in-place resize deferred by kubelet, will apply when conditions allow")
			return false, false, nil
		case "":
			logger.V(1).Info("pod already at target resources, no in-place patch needed")
			return false, false, nil
		default:
			// A verdict this version doesn't know. Leaving the pod to the
			// kubelet is the safe default, but say so instead of pretending
			// the pod is settled.
			logger.Info("unrecognized resize verdict, leaving pod to the kubelet", "verdict", verdict)
			return false, false, nil
		}
	}

	// Patch regular containers and sidecar init containers in two separate
	// /resize calls. Sidecar in-place resize needs feature gates beyond
	// InPlacePodVerticalScaling and may be rejected; isolating it from the
	// regular-container patch ensures a sidecar rejection cannot block the
	// regular containers' resize.
	if regChanged {
		pod.Spec.Containers = containers
		ok, err := p.submitInPlaceResize(ctx, pod, base)
		if err != nil {
			return false, false, err
		}
		if !ok {
			// Resize not applied: rejected as invalid for this pod, or pod
			// gone — the pod object was reverted to the server's spec. The
			// strategy decides whether to evict (so the webhook re-injects on
			// the replacement) or skip.
			logger.Info(strat.unappliedLog)
			evicted, err = strat.onUnapplied(ctx, pod, recs)
			return false, evicted, err
		}
		applied = true
	}

	if initChanged {
		if p.applySidecarResize(ctx, pod, base, initContainers) {
			applied = true
		}
	}
	return applied, false, nil
}

// recyclePods drives running pods toward the updated resource spec.
// On clusters that support InPlacePodVerticalScaling the pod's resources are
// patched directly (zero restart). On older clusters each stale pod is evicted
// via the Eviction API so the workload controller replaces it from the updated
// template; PDB-blocked pods are skipped and retried on the next reconcile.
//
// In the eviction path, pods are processed one at a time: after each
// successful eviction the patcher waits for the workload controller to bring
// the replacement pod back to Ready before continuing. If any pod matching
// the selector enters CrashLoopBackOff during the wait, the loop aborts so a
// bad recommendation cannot cascade through the whole workload.
//
// For StatefulSets pods are processed in descending ordinal order so we
// don't violate the ordinal invariant (e.g. evict pod-1 before pod-0 is
// running again).
//
// Pods matching the selector but not owned by the target workload are
// skipped: the opt-in contract is per-workload (annotation on the pod
// template), so a bystander pod that merely shares the labels must never be
// resized or evicted.
func (p *Patcher) recyclePods(ctx context.Context, target TargetWorkload, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation, o recycleOptions) error {
	logger := log.FromContext(ctx).WithValues("namespace", namespace, "selector", selector.String())

	var podList corev1.PodList
	if err := p.client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	strategy := "eviction"
	if p.inPlace {
		strategy = "inPlace"
	}
	logger.V(1).Info("listed pods for recycle", "count", len(podList.Items), "strategy", strategy)

	// Keep only pods whose controller ownerRef chain resolves to the target
	// workload. StatefulSet/DaemonSet/Job pods reference the workload
	// directly; Deployment/Rollout pods go through an intermediate
	// ReplicaSet, memoized in rsOwned so each ReplicaSet costs at most one
	// GET per recycle pass. Then build a stable ordering of pod pointers:
	// for StatefulSets, descending ordinal — matches the StatefulSet
	// controller's update sequence; for everything else, alphabetical by
	// name keeps reconciles deterministic across runs (helpful for tests,
	// logs, and rate-limited evictions).
	rsOwned := map[string]bool{}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		if target.UID != "" {
			owned, err := PodOwnedByWorkload(ctx, p.client, pod, target.UID, rsOwned)
			if err != nil {
				return fmt.Errorf("resolving owner of pod %s: %w", pod.Name, err)
			}
			if !owned {
				logger.Info("skipping pod matching selector but not owned by target workload",
					"pod", pod.Name, "targetKind", target.Kind, "targetName", target.Name)
				continue
			}
		}
		pods = append(pods, pod)
	}
	sortPodsForRecycle(pods)

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pod.DeletionTimestamp != nil {
			logger.V(1).Info("skipping terminating pod", "pod", pod.Name)
			skipped++
			continue
		}
		// In-place resize requires a Running pod (kubelet must observe the
		// patch). Eviction also works on Pending pods — a pod stuck Pending
		// because its request is too large is exactly the case we want to
		// recycle so the webhook can re-inject the smaller recommendation.
		// Succeeded/Failed pods are not worth touching in either mode.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			logger.V(1).Info("skipping terminal pod", "pod", pod.Name, "phase", pod.Status.Phase)
			skipped++
			continue
		}
		if p.inPlace && pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping non-Running pod for in-place resize", "pod", pod.Name, "phase", pod.Status.Phase)
			skipped++
			continue
		}
		// Clamp per-pod: a pod's containers may carry different current
		// allocations than their siblings (e.g. a partially-applied prior
		// resize), so the downsize threshold must be evaluated against this
		// pod's actual resources, not the workload template.
		podRecs := ClampRecsToTolerance(podContainers(pod), recs, o.tol)
		observeSuppressed(recs, podRecs, o.observe)
		var (
			evicted bool
			err     error
		)
		if p.inPlace {
			// The in-place path can still evict the pod inline as a
			// fallback (Infeasible status, or IsInvalid response indicating
			// the InPlacePodVerticalScaling feature gate is off). When it
			// does, the outer loop must run the same throttle/wait the
			// pure-eviction branch uses below — otherwise the first
			// fallback eviction skips the wait entirely.
			evicted, err = p.patchPodInPlace(ctx, pod, podRecs, o.ignoreSafeToEvict)
		} else {
			// Eviction path: evict, then wait for the replacement before
			// moving on so we don't take down the whole workload at once.
			evicted, err = p.evictPod(ctx, pod, podRecs, o.ignoreSafeToEvict)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		processed++
		if !evicted {
			continue
		}
		if waitErr := p.waitForReplacement(ctx, namespace, selector, pod.Name, pod.UID); waitErr != nil {
			// CrashLoopBackOff in the selector means the recommendation is
			// likely making things worse — stop evicting and surface the
			// error so the next reconcile can re-evaluate. Timeouts get the
			// same treatment: continuing to evict an unhealthy workload only
			// amplifies the damage.
			errs = append(errs, fmt.Errorf("after evicting %s: %w", pod.Name, waitErr))
			logger.Info("halting eviction loop for this reconcile", "reason", waitErr.Error())
			break
		}
	}
	logger.Info("recycle pass complete", "processed", processed, "skipped", skipped, "errors", len(errs), "strategy", strategy)
	return errors.Join(errs...)
}

// patchPodInPlace patches a single pod's container resources without restarting it.
//
// When the pod spec already carries the target resources, the kubelet's
// verdict on the staged resize (PodResizePending / PodResizeInProgress
// conditions) decides what happens:
//
//   - Infeasible: the node cannot satisfy the resources; fall back to eviction
//     so the scheduler can place the replacement pod elsewhere. The eviction
//     is unconditional — the spec matches the recommendation, so a
//     spec-staleness gate would wrongly skip it.
//   - Error: the kubelet failed while actuating the accepted resize and does
//     not retry on its own; same eviction fallback as Infeasible.
//   - Deferred: the kubelet accepted the request but is waiting for the right
//     conditions (e.g. room freed by another pod terminating). Skip for now —
//     the kubelet will apply it without further action from us.
//   - No pending resize: the pod is at target; nothing to do.
//
// When the spec differs from the recommendation, the new target is submitted
// regardless of any verdict on the previous one — the kubelet re-evaluates.
//
// Returns (evicted, err). evicted=true means the in-place path bailed out
// to an Eviction (Infeasible/Error verdict, or per-pod Invalid rejection)
// and the caller must run the same throttle/wait the pure-eviction branch
// uses, so the loop doesn't take down the whole workload at once.
func (p *Patcher) patchPodInPlace(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, ignoreSafeToEvict bool) (bool, error) {
	_, evicted, err := p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unsatisfiableLog: "staged in-place resize cannot complete, falling back to eviction",
		unappliedLog:     "falling back to eviction",
		// Node can't deliver the staged resize: evict so the scheduler
		// places the replacement (with webhook-injected resources)
		// elsewhere. submitEviction, not evictPod: the spec already matches
		// the recommendation, so evictPod's staleness gate would skip it.
		onUnsatisfiable: func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error) {
			return p.submitEviction(ctx, pod, "in-place resize verdict "+verdict, ignoreSafeToEvict)
		},
		// API server rejected this pod's resize (per-pod validation
		// failure, e.g. QoS class change): fall back to eviction so the
		// webhook re-injects the new requests on the replacement. The
		// sidecar resize is skipped — the replacement pod is on its way.
		onUnapplied: func(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error) {
			return p.evictPod(ctx, pod, recs, ignoreSafeToEvict)
		},
	})
	return evicted, err
}

// submitInPlaceResize submits an in-place patch to the /resize subresource
// (k8s ≥ 1.33) to bring the pod's staged spec changes from base to pod.
//
// The subresource is always served on supported clusters, so an Invalid
// rejection is a per-pod validation failure (QoS class would change, memory
// limit decrease with a NotRequired resize policy, ...) — only this pod is
// affected. NotFound means the pod itself is gone.
//
// Returns:
//
//	(true,  nil) — patch applied successfully
//	(false, nil) — patch not applied: rejected as Invalid (pod reverted to
//	               base), or the pod no longer exists. The caller decides
//	               whether to evict.
//	(false, err) — unrecoverable patch failure
func (p *Patcher) submitInPlaceResize(ctx context.Context, pod, base *corev1.Pod) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	logger.V(1).Info("attempting in-place resize via /resize subresource")
	err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(base))
	switch {
	case err == nil:
		logger.Info("in-place resize applied")
		return true, nil
	case apierrors.IsInvalid(err):
		logger.Info("in-place resize rejected as invalid for this pod (e.g. QoS class change)", "err", err.Error())
		revertStagedResize(pod, base)
		return false, nil
	case apierrors.IsNotFound(err):
		// Pod deleted between List and patch — mirror evictPod's NotFound
		// handling and treat it as a no-op instead of surfacing a
		// reconcile error.
		logger.Info("pod gone before in-place resize, skipping")
		return false, nil
	default:
		return false, err
	}
}

// revertStagedResize restores the container specs staged for an in-place
// resize back to the server's last-known state, so a later staleness check
// (evictPod) compares the recommendation against what actually runs.
func revertStagedResize(pod, base *corev1.Pod) {
	pod.Spec.Containers = base.Spec.Containers
	pod.Spec.InitContainers = base.Spec.InitContainers
}

// applySidecarResize submits an in-place /resize for staged sidecar init
// container changes, reporting whether it was accepted. Sidecar in-place
// resize is best-effort: it needs feature gates beyond
// InPlacePodVerticalScaling and may be rejected, so any error is logged and
// the original sidecar resources are restored. The replacement pod will pick up
// the new resources via the webhook at next pod creation. A baseline
// reflecting the (possibly already applied) regular-container changes is
// built so the sidecar diff is the only remaining delta.
func (p *Patcher) applySidecarResize(ctx context.Context, pod, base *corev1.Pod, initContainers []corev1.Container) bool {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)
	sidecarBase := pod.DeepCopy()
	sidecarBase.Spec.InitContainers = base.Spec.InitContainers
	pod.Spec.InitContainers = initContainers
	if err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(sidecarBase)); err != nil {
		logger.Info("sidecar in-place resize not accepted, will apply at next pod creation",
			"err", err.Error())
		pod.Spec.InitContainers = base.Spec.InitContainers
		return false
	}
	logger.Info("sidecar in-place resize applied")
	return true
}

// evictPod evicts a pod if it is running stale resources, so the workload
// controller replaces it from the updated template.
//
// A 429 (Too Many Requests) response from the Eviction API means a
// PodDisruptionBudget is blocking the eviction. The pod is skipped silently —
// it will be retried on the next reconcile cycle.
//
// Returns (evicted, err). evicted=true means the apiserver accepted the
// Eviction request; the caller should wait for the replacement before
// continuing. evicted=false covers no-op cases (pod already fresh, gone, or
// PDB-blocked) where no wait is required.
func (p *Patcher) evictPod(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, ignoreSafeToEvict bool) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	if !podIsStale(pod, recs) {
		logger.V(1).Info("pod already running recommended resources, eviction skipped")
		return false, nil // already running with the recommended resources
	}
	return p.submitEviction(ctx, pod, "stale resources", ignoreSafeToEvict)
}

// submitEviction creates the Eviction without any staleness gate. Used
// directly for unsatisfiable in-place resizes (Infeasible/Error), where the
// pod spec already matches the recommendation (the resize was accepted but
// can't land on the node) and a spec-based staleness check would wrongly
// skip the eviction. why is logged so operators can tell a staleness
// eviction from a resize-fallback one. Pods annotated safe-to-evict=false
// are skipped unless ignoreSafeToEvict is set — this is the single gate for
// both eviction triggers.
func (p *Patcher) submitEviction(ctx context.Context, pod *corev1.Pod, why string, ignoreSafeToEvict bool) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	if !ignoreSafeToEvict && pod.Annotations[SafeToEvictAnnotation] == "false" {
		// Same contract as a PDB block: skip without error, no replacement
		// wait; the pod is re-considered on the next reconcile.
		logger.Info("eviction skipped: pod annotated safe-to-evict=false",
			"reason", why,
			"override", "spec.rightSizing.update.eviction.ignoreAutoscalerSafeToEvictAnnotations")
		return false, nil
	}

	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	logger.Info("evicting pod", "reason", why)
	err := p.client.SubResource("eviction").Create(ctx, pod, eviction)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		logger.Info("pod already deleted, skipping eviction")
		return false, nil
	}
	if apierrors.IsTooManyRequests(err) {
		// PDB is blocking — log and move on; next reconcile will retry.
		logger.Info("eviction blocked by PodDisruptionBudget, will retry")
		return false, nil
	}
	return false, err
}

// errCrashLoopBackOff signals that a pod in the workload's selector has
// entered CrashLoopBackOff during the post-eviction wait. The recycle loop
// aborts when this happens so a bad recommendation cannot cascade across the
// whole workload.
var errCrashLoopBackOff = errors.New("pod in CrashLoopBackOff; aborting eviction loop")

// waitForReplacement blocks until either:
//   - the evicted pod is gone AND every other pod matching the selector has
//     settled (Ready, or terminating, or Succeeded), OR
//   - p.readyTimeout fires, OR
//   - any pod in the selector enters CrashLoopBackOff (likely a bad
//     recommendation — surface so the caller stops evicting).
//
// Rather than comparing against a frozen Ready-count baseline, we wait for
// the selector to be quiescent — no peer is Pending or running-but-NotReady.
// This handles HPA scale-down naturally: if the autoscaler decides to not
// replace the evicted pod, the remaining peers stay Ready and the wait
// returns immediately. A stuck or slow replacement keeps a Pending peer in
// the list, blocking until the timeout.
//
// The evicted pod is identified by UID, not by name. For Deployments the
// replacement gets a fresh name (and UID) so name would work too, but
// StatefulSets reuse the evicted pod's name for the replacement (e.g.
// `web-2` reappears with a new UID). Keying on UID is the only way to tell
// "still the original, terminating" from "fresh replacement, same name" in
// the StatefulSet case.
func (p *Patcher) waitForReplacement(ctx context.Context, namespace string, selector klabels.Selector, evictedName string, evictedUID types.UID) error {
	logger := log.FromContext(ctx).WithValues("namespace", namespace, "evictedName", evictedName, "evictedUID", evictedUID)

	deadline := time.Now().Add(p.readyTimeout)
	for {
		var list corev1.PodList
		if err := p.client.List(ctx, &list,
			client.InNamespace(namespace),
			client.MatchingLabelsSelector{Selector: selector},
		); err != nil {
			return fmt.Errorf("listing pods while waiting for replacement: %w", err)
		}

		evictedGone := true
		pending := 0
		for i := range list.Items {
			pod := &list.Items[i]
			if hasCrashLoopBackOff(pod) {
				logger.Info("crashloop detected during replacement wait", "pod", pod.Name)
				return fmt.Errorf("%w: %s", errCrashLoopBackOff, pod.Name)
			}
			// A pod sharing the evicted name but carrying a different UID is
			// the StatefulSet/etc replacement — count it as a peer, not as
			// "the evicted pod still hanging around".
			if pod.UID == evictedUID && evictedUID != "" {
				evictedGone = false
				continue
			}
			// Fallback when the caller didn't supply a UID (e.g. older
			// callers, or a pod object that never carried one): rely on
			// name uniqueness, accepting that StatefulSet replacements may
			// be misclassified during a brief window.
			if evictedUID == "" && pod.Name == evictedName {
				evictedGone = false
				continue
			}
			if isProgressing(pod) {
				pending++
			}
		}

		if evictedGone && pending == 0 {
			logger.V(1).Info("workload quiescent; resuming eviction loop")
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for replacement (evictedGone=%v, pending=%d)",
				p.readyTimeout, evictedGone, pending)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.readyPollInterval):
		}
	}
}

// isProgressing reports whether a pod is still on its way to a steady state.
// Pending pods (scheduler / image pull / init container running) and Running
// pods without PodReady=True count as progressing. Terminating pods
// (DeletionTimestamp set) and pods in Succeeded/Failed phase do NOT count —
// they have already reached an end state, and waiting on them would deadlock
// the eviction loop whenever a peer is in the process of being recycled.
func isProgressing(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	case corev1.PodPending:
		return true
	}
	return !isPodReady(pod)
}

// hasCrashLoopBackOff reports whether any container in the pod is currently
// waiting in CrashLoopBackOff. Init and ephemeral container statuses are
// included — a sidecar crashloop is just as fatal to the pod's usefulness
// as a regular container crashloop.
func hasCrashLoopBackOff(pod *corev1.Pod) bool {
	return containersCrashing(pod.Status.ContainerStatuses) ||
		containersCrashing(pod.Status.InitContainerStatuses) ||
		containersCrashing(pod.Status.EphemeralContainerStatuses)
}

func containersCrashing(cs []corev1.ContainerStatus) bool {
	return slices.ContainsFunc(cs, func(c corev1.ContainerStatus) bool {
		return c.State.Waiting != nil && c.State.Waiting.Reason == "CrashLoopBackOff"
	})
}

// isPodReady mirrors the k8s.io/kubernetes podutil helper: the pod is Ready
// when it carries a PodReady condition with status=True.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// sortPodsForRecycle imposes a deterministic order on the eviction loop. For
// StatefulSet-owned pods we follow the StatefulSet update sequence (highest
// ordinal first). For everything else we sort by name so that reconciles are
// reproducible across runs.
//
// StatefulSet detection inspects every pod in the slice, not just pods[0]:
// an orphan/relabel/transiently-missing ownerRef on the first pod must not
// downgrade the slice to alphabetical ordering, which would mis-order names
// like web-0/web-1/web-10/web-2 and violate StatefulSet update invariants.
func sortPodsForRecycle(pods []*corev1.Pod) {
	if len(pods) == 0 {
		return
	}
	if anyOwnedByStatefulSet(pods) {
		slices.SortStableFunc(pods, func(a, b *corev1.Pod) int {
			oa, aok := podOrdinal(a)
			ob, bok := podOrdinal(b)
			// Fall back to name-desc when an ordinal can't be parsed so
			// the sort stays total even with oddly-named pods.
			if !aok || !bok {
				return cmp.Compare(b.Name, a.Name)
			}
			return cmp.Compare(ob, oa)
		})
		return
	}
	slices.SortStableFunc(pods, func(a, b *corev1.Pod) int {
		return cmp.Compare(a.Name, b.Name)
	})
}

// anyOwnedByStatefulSet reports whether any pod in the slice carries a
// controller ownerRef pointing at a StatefulSet. We treat the whole slice as
// StatefulSet-owned even if a single pod has the ref, because pods returned
// by a label selector are by construction peers of the same workload.
func anyOwnedByStatefulSet(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if IsOwnedByKind(pod.OwnerReferences, "StatefulSet") {
			return true
		}
	}
	return false
}

// podOrdinal extracts the trailing integer from a StatefulSet pod name
// (e.g. "web-3" → 3). Returns (0, false) if the name has no parseable suffix.
func podOrdinal(pod *corev1.Pod) (int, bool) {
	idx := strings.LastIndex(pod.Name, "-")
	if idx < 0 || idx == len(pod.Name)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(pod.Name[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// podIsStale returns true if any container in the pod has different CPU or
// memory requests than the recommendation, meaning the pod was created from
// an outdated template and should be replaced.
//
// Init containers contribute to staleness only when they are restartable
// sidecars (restartPolicy=Always). Classic init containers have already
// exited by the time a pod is Running, so drift in their requests cannot be
// addressed by recycling — the new requests will land via webhook injection
// on the next pod creation.
func podIsStale(pod *corev1.Pod, recs map[string]ContainerRecommendation) bool {
	if anyContainerStale(pod.Spec.Containers, recs) {
		return true
	}
	for _, c := range pod.Spec.InitContainers {
		if !isRestartableInitContainer(c) {
			continue
		}
		if anyContainerStale([]corev1.Container{c}, recs) {
			return true
		}
	}
	return false
}

func anyContainerStale(cs []corev1.Container, recs map[string]ContainerRecommendation) bool {
	for _, c := range cs {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		if !ContainerMatches(c.Resources, rec) {
			return true
		}
	}
	return false
}

// isRestartableInitContainer reports whether an init container is a sidecar
// (restartPolicy=Always per KEP-753), meaning it runs for the pod's lifetime
// and is eligible for in-place resize on supported clusters.
func isRestartableInitContainer(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// ApplyRecommendationsToPodSpec mutates the regular and init containers in a
// PodSpec to match the given recommendations, returning whether anything
// changed. Use this for template mutation (e.g. CronJob.JobTemplate) where
// every container is fair game; for live-pod patching go through Patcher.
func ApplyRecommendationsToPodSpec(spec *corev1.PodSpec, recs map[string]ContainerRecommendation) bool {
	changed := false
	for i := range spec.Containers {
		if applyRecToContainer(&spec.Containers[i], recs[spec.Containers[i].Name]) {
			changed = true
		}
	}
	for i := range spec.InitContainers {
		if applyRecToContainer(&spec.InitContainers[i], recs[spec.InitContainers[i].Name]) {
			changed = true
		}
	}
	return changed
}

// applyRecsFiltered copies the container slice and applies the matching
// recommendation to every container the predicate accepts. Returns the new
// slice and whether anything changed. A nil predicate accepts all containers.
//
// Use a non-nil predicate for init containers — only restartable sidecars
// should be patched in place; classic init containers have already exited in
// a Running pod, so patching their resources is a no-op at best and an API
// error on some versions.
func applyRecsFiltered(in []corev1.Container, recs map[string]ContainerRecommendation, accept func(corev1.Container) bool) ([]corev1.Container, bool) {
	if len(in) == 0 {
		return in, false
	}
	// Deep copy each element: a shallow copy would share the ResourceList
	// maps with the input slice, so applyRecToContainer would mutate the
	// caller's pod spec in place — leaking e.g. sidecar changes into the
	// regular-container /resize patch that diffs against the original spec.
	out := make([]corev1.Container, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	changed := false
	for i := range out {
		if accept != nil && !accept(out[i]) {
			continue
		}
		if applyRecToContainer(&out[i], recs[out[i].Name]) {
			changed = true
		}
	}
	return out, changed
}

// applyRecToContainer applies a single recommendation to one container in
// place, returning whether any value actually changed. A nil/zero rec is a
// no-op, and so is a rec the container already satisfies — the comparison
// mirrors ContainerMatches (numeric, unset reads as zero), so the in-place
// resize path skips pods that are already at the recommended size instead of
// submitting empty patches every reconcile.
func applyRecToContainer(c *corev1.Container, rec ContainerRecommendation) bool {
	changed := setQuantity(&c.Resources.Requests, corev1.ResourceCPU, rec.CPURequest)
	if applyLimit(&c.Resources.Limits, corev1.ResourceCPU, rec.CPULimit, rec.RemoveCPULimit) {
		changed = true
	}
	if setQuantity(&c.Resources.Requests, corev1.ResourceMemory, rec.MemoryRequest) {
		changed = true
	}
	if applyLimit(&c.Resources.Limits, corev1.ResourceMemory, rec.MemoryLimit, rec.RemoveMemoryLimit) {
		changed = true
	}
	return changed
}

// setQuantity sets list[name] = *q, returning whether the value changed. A nil
// q means "leave this dimension alone". An absent key compares as a zero
// Quantity, matching requestMatches/limitMatches in match.go. The map is
// allocated lazily so an all-no-op rec leaves nil maps untouched.
func setQuantity(list *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity) bool {
	if q == nil {
		return false
	}
	if cur := (*list)[name]; cur.Cmp(*q) == 0 {
		return false
	}
	if *list == nil {
		*list = corev1.ResourceList{}
	}
	(*list)[name] = *q
	return true
}

// applyLimit applies the limit dimension of a rec: remove=true deletes the
// entry (a change only when a non-zero limit was present, mirroring
// limitMatches), otherwise it defers to setQuantity. remove takes precedence
// over a non-nil limit.
func applyLimit(list *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity, remove bool) bool {
	if !remove {
		return setQuantity(list, name, q)
	}
	cur, ok := (*list)[name]
	if !ok {
		return false
	}
	delete(*list, name)
	return !cur.IsZero()
}
