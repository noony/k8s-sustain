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

// ContainerRecommendation holds computed resource changes for a single
// container. Its field sequence mirrors v1alpha1.ContainerRecommendation so
// wlrcache can convert between the two with a plain struct conversion; the
// compiler rejects any drift.
type ContainerRecommendation struct {
	CPURequest        *resource.Quantity
	MemoryRequest     *resource.Quantity
	CPULimit          *resource.Quantity
	MemoryLimit       *resource.Quantity
	RemoveCPULimit    bool
	RemoveMemoryLimit bool
}

// SafeToEvictAnnotation is the cluster-autoscaler annotation whose literal
// value "false" blocks eviction. In-place resizes are never gated by it.
const SafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// PodCounts reports live pods still differing from the recommendation after a
// pass (Stale) out of all live pods owned by the workload (Total).
type PodCounts struct {
	Total int
	Stale int
}

func countable(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

func countPods(pods []*corev1.Pod, recs map[string]ContainerRecommendation, tol Tolerance, fixed map[types.UID]bool) PodCounts {
	var pc PodCounts
	for _, pod := range pods {
		if !countable(pod) {
			continue
		}
		pc.Total++
		if !fixed[pod.UID] && podIsStale(pod, ClampRecsToTolerance(podContainers(pod), recs, tol)) {
			pc.Stale++
		}
	}
	return pc
}

func podContainers(pod *corev1.Pod) []corev1.Container {
	out := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	out = append(out, pod.Spec.Containers...)
	out = append(out, pod.Spec.InitContainers...)
	return out
}

// Patcher drives running pods toward the latest recommendation, either via
// the pods/resize subresource (k8s >= 1.33) or by PDB-respecting eviction so
// the webhook re-injects on the replacement. It never modifies workload
// specs, which keeps it GitOps-safe.
type Patcher struct {
	client  client.Client
	inPlace bool

	readyPollInterval time.Duration
	readyTimeout      time.Duration
}

// Option configures a Patcher at construction time.
type Option func(*Patcher)

// WithReadyPollInterval sets how often the patcher polls pod state while
// waiting for a replacement after an eviction.
func WithReadyPollInterval(d time.Duration) Option {
	return func(p *Patcher) {
		if d > 0 {
			p.readyPollInterval = d
		}
	}
}

// WithReadyTimeout caps how long the patcher waits for a replacement pod
// to become Ready after an eviction.
func WithReadyTimeout(d time.Duration) Option {
	return func(p *Patcher) {
		if d > 0 {
			p.readyTimeout = d
		}
	}
}

// The 5m timeout covers node provisioning plus image pull on a fresh node,
// so a normal scale-up does not read as a failed replacement.
const (
	defaultReadyPollInterval = 2 * time.Second
	defaultReadyTimeout      = 5 * time.Minute
)

// New returns a Patcher; inPlace enables the pods/resize path.
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

// resizeInPlaceOnly resizes running pods in place and never evicts, for kinds
// where eviction would destroy work nothing redoes. Without in-place support
// it only counts.
func (p *Patcher) resizeInPlaceOnly(ctx context.Context, pods []*corev1.Pod, recs map[string]ContainerRecommendation, s ApplySettings) (Outcome, error) {
	out := Outcome{InPlaceOnly: true}
	logger := log.FromContext(ctx)
	if !p.inPlace || s.DryRun {
		if !p.inPlace {
			logger.V(1).Info("in-place resize disabled on this cluster; deferring to next pod creation via webhook")
		}
		out.Pods = countPods(pods, recs, s.Tolerance, nil)
		return out, nil
	}
	fixed := map[types.UID]bool{}

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping pod", "pod", pod.Name, "phase", pod.Status.Phase, "deleting", pod.DeletionTimestamp != nil)
			skipped++
			continue
		}
		podRecs := ClampRecsToTolerance(podContainers(pod), recs, s.Tolerance)
		observeSuppressed(recs, podRecs, out.countSuppressed)
		applied, err := p.resizePodInPlaceNoEvict(ctx, pod, podRecs)
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		if applied {
			out.Changed++
			fixed[pod.UID] = true
		}
		processed++
	}
	logger.Info("in-place resize pass complete", "processed", processed, "skipped", skipped, "resized", out.Changed, "errors", len(errs))
	err := errors.Join(errs...)
	if err == nil {
		out.Pods = countPods(pods, recs, s.Tolerance, fixed)
	}
	return out, err
}

// unapplyStrategy decides what happens when an in-place resize cannot be
// applied. Both hooks return (evicted, err).
type unapplyStrategy struct {
	unsatisfiableLog string
	unappliedLog     string
	onUnsatisfiable  func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error)
	onUnapplied      func(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error)
}

// resizePendingReason returns the kubelet's verdict on a staged resize
// (Infeasible, Deferred, Error, ...) or "" when none is pending. A pending
// verdict wins over an in-progress error since it refers to the latest spec.
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

// resizePodInPlaceNoEvict mirrors patchPodInPlace but never evicts, for
// in-place-only kinds.
func (p *Patcher) resizePodInPlaceNoEvict(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error) {
	applied, _, err := p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unsatisfiableLog: "staged in-place resize cannot complete for short-lived pod, skipping (next run will pick up new resources)",
		unappliedLog:     "skipping short-lived pod (next run via webhook)",
		onUnsatisfiable: func(context.Context, *corev1.Pod, string) (bool, error) {
			return false, nil
		},
		onUnapplied: func(context.Context, *corev1.Pod, map[string]ContainerRecommendation) (bool, error) {
			return false, nil
		},
	})
	return applied, err
}

// resizePodInPlaceWith is the in-place resize body shared by patchPodInPlace
// and resizePodInPlaceNoEvict. It returns (applied, evicted, err).
//
// The kubelet's verdict is only consulted when the spec already carries the
// target: a verdict refers to the staged resize, so a changed recommendation
// is submitted and re-evaluated.
func (p *Patcher) resizePodInPlaceWith(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, strat unapplyStrategy) (applied, evicted bool, err error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	base := pod.DeepCopy()
	containers, regChanged := applyRecsFiltered(pod.Spec.Containers, recs, nil)
	initContainers, initChanged := applyRecsFiltered(pod.Spec.InitContainers, recs, isRestartableInitContainer)
	if !regChanged && !initChanged {
		switch verdict := resizePendingReason(pod); verdict {
		case corev1.PodReasonInfeasible, corev1.PodReasonError:
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
			logger.Info("unrecognized resize verdict, leaving pod to the kubelet", "verdict", verdict)
			return false, false, nil
		}
	}

	// Sidecars are resized in a separate /resize call: they need extra feature
	// gates and a rejection must not block the regular containers.
	if regChanged {
		pod.Spec.Containers = containers
		ok, err := p.submitInPlaceResize(ctx, pod, base)
		if err != nil {
			return false, false, err
		}
		if !ok {
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

// recycle resizes or evicts stale pods one at a time, waiting for each
// replacement and aborting on CrashLoopBackOff so a bad recommendation
// cannot cascade through the workload, within a pass or across passes.
func (p *Patcher) recycle(ctx context.Context, set podSet, recs map[string]ContainerRecommendation, s ApplySettings) (Outcome, error) {
	var out Outcome
	logger := log.FromContext(ctx).WithValues("namespace", set.namespace, "selector", set.selector.String())
	strategy := "eviction"
	if p.inPlace {
		strategy = "inPlace"
	}
	logger.V(1).Info("listed pods for recycle", "count", len(set.pods), "strategy", strategy)

	pods := set.pods
	sortPodsForRecycle(pods)
	if s.DryRun {
		out.Pods = countPods(pods, recs, s.Tolerance, nil)
		return out, nil
	}
	fixed := map[types.UID]bool{}
	// Read before the loop: an in-place resize rewrites the spec of the pod it
	// resizes, and a stale pod resized this pass is no proof against the
	// recommendation.
	gate := evictionGate{ignoreSafeToEvict: s.IgnoreSafeToEvict, halted: updatedPodCrashLooping(pods, recs, s.Tolerance)}

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if pod.DeletionTimestamp != nil {
			logger.V(1).Info("skipping terminating pod", "pod", pod.Name)
			skipped++
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			logger.V(1).Info("skipping terminal pod", "pod", pod.Name, "phase", pod.Status.Phase)
			skipped++
			continue
		}
		// Clamp per pod: siblings may carry different current allocations.
		podRecs := ClampRecsToTolerance(podContainers(pod), recs, s.Tolerance)
		observeSuppressed(recs, podRecs, out.countSuppressed)
		var (
			applied, evicted bool
			err              error
		)
		// A pod that is not Running cannot be resized in place, so it is evicted
		// in both modes: one stuck Pending on an oversized request is exactly
		// what the webhook should re-inject.
		if p.inPlace && pod.Status.Phase == corev1.PodRunning {
			applied, evicted, err = p.patchPodInPlace(ctx, pod, podRecs, gate)
		} else {
			evicted, err = p.evictPod(ctx, pod, podRecs, gate)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		if errors.Is(err, errCrashLoopBackOff) {
			logger.Info("halting eviction loop for this reconcile", "reason", err.Error())
			break
		}
		processed++
		if applied || evicted {
			out.Changed++
			fixed[pod.UID] = true
		}
		if !evicted {
			continue
		}
		if waitErr := p.waitForReplacement(ctx, set.namespace, set.selector, pod.Name, pod.UID); waitErr != nil {
			errs = append(errs, fmt.Errorf("after evicting %s: %w", pod.Name, waitErr))
			logger.Info("halting eviction loop for this reconcile", "reason", waitErr.Error())
			break
		}
	}
	logger.Info("recycle pass complete", "processed", processed, "skipped", skipped, "changed", out.Changed, "errors", len(errs), "strategy", strategy)
	err := errors.Join(errs...)
	if err == nil {
		out.Pods = countPods(pods, recs, s.Tolerance, fixed)
	}
	return out, err
}

// patchPodInPlace resizes a pod in place, falling back to eviction when the
// resize is Infeasible/Error or rejected as Invalid. Returns (applied,
// evicted, err).
func (p *Patcher) patchPodInPlace(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, gate evictionGate) (bool, bool, error) {
	return p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unsatisfiableLog: "staged in-place resize cannot complete, falling back to eviction",
		unappliedLog:     "falling back to eviction",
		// submitEviction, not evictPod: the spec already matches the
		// recommendation, so evictPod's staleness gate would skip it.
		onUnsatisfiable: func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error) {
			return p.submitEviction(ctx, pod, "in-place resize verdict "+verdict, gate)
		},
		onUnapplied: func(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) (bool, error) {
			return p.evictPod(ctx, pod, recs, gate)
		},
	})
}

// submitInPlaceResize patches the /resize subresource with the diff from
// base to pod. It returns (false, nil) when the resize was rejected as
// Invalid (pod reverted to base) or the pod is gone.
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
		logger.Info("pod gone before in-place resize, skipping")
		return false, nil
	default:
		return false, err
	}
}

// revertStagedResize restores the server's container specs so a later staleness
// check compares against what actually runs.
func revertStagedResize(pod, base *corev1.Pod) {
	pod.Spec.Containers = base.Spec.Containers
	pod.Spec.InitContainers = base.Spec.InitContainers
}

// applySidecarResize submits a best-effort /resize for sidecar init
// containers; on rejection the original resources are restored.
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

// evictionGate is what can hold back an eviction in one recycle pass.
type evictionGate struct {
	ignoreSafeToEvict bool
	// halted, when set, is returned instead of evicting.
	halted error
}

// evictPod evicts a pod running stale resources. Returns (evicted, err);
// evicted=false covers pods already fresh, gone, or PDB-blocked.
func (p *Patcher) evictPod(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, gate evictionGate) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	if !podIsStale(pod, recs) {
		logger.V(1).Info("pod already running recommended resources, eviction skipped")
		return false, nil
	}
	return p.submitEviction(ctx, pod, "stale resources", gate)
}

// submitEviction creates the Eviction without a staleness gate. It is the
// single gate check for both eviction triggers.
func (p *Patcher) submitEviction(ctx context.Context, pod *corev1.Pod, why string, gate evictionGate) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	if !gate.ignoreSafeToEvict && pod.Annotations[SafeToEvictAnnotation] == "false" {
		logger.Info("eviction skipped: pod annotated safe-to-evict=false",
			"reason", why,
			"override", "spec.rightSizing.update.eviction.ignoreAutoscalerSafeToEvictAnnotations")
		return false, nil
	}
	if gate.halted != nil {
		return false, gate.halted
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
		// 429 means a PodDisruptionBudget is blocking; retried next reconcile.
		logger.Info("eviction blocked by PodDisruptionBudget, will retry")
		return false, nil
	}
	return false, err
}

// errCrashLoopBackOff aborts the recycle loop when a pod in the selector
// enters CrashLoopBackOff during the post-eviction wait, or when the pass
// would evict while a pod already running the recommendation crash-loops.
var errCrashLoopBackOff = errors.New("pod in CrashLoopBackOff; aborting eviction loop")

// updatedPodCrashLooping returns the error that halts a pass's evictions when
// a live pod already running recs is in CrashLoopBackOff. The post-eviction
// wait only sees crash-loops that start during the pass that evicted, so
// without this each pass would evict one more pod before halting on the same
// crash-looping replacement. A stale crash-looping pod halts nothing: the new
// numbers may be its fix.
func updatedPodCrashLooping(pods []*corev1.Pod, recs map[string]ContainerRecommendation, tol Tolerance) error {
	for _, pod := range pods {
		if !countable(pod) || !hasCrashLoopBackOff(pod) {
			continue
		}
		if podIsStale(pod, ClampRecsToTolerance(podContainers(pod), recs, tol)) {
			continue
		}
		return fmt.Errorf("%w: %s already runs the recommendation", errCrashLoopBackOff, pod.Name)
	}
	return nil
}

// waitForReplacement blocks until the evicted pod is gone and the selector
// is quiescent, the timeout fires, or a pod enters CrashLoopBackOff.
// Waiting for quiescence rather than a Ready-count baseline lets HPA
// scale-down through. The evicted pod is keyed by UID because StatefulSets
// reuse the name for the replacement.
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
			if pod.UID == evictedUID && evictedUID != "" {
				evictedGone = false
				continue
			}
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

// isProgressing reports whether a pod is Pending or Running but not Ready.
// Terminating and terminal pods do not count, or the wait would deadlock.
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

// hasCrashLoopBackOff reports whether any container, init or ephemeral
// container is waiting in CrashLoopBackOff.
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

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// sortPodsForRecycle orders StatefulSet pods by descending ordinal, matching
// the StatefulSet update sequence, and everything else by name.
func sortPodsForRecycle(pods []*corev1.Pod) {
	if len(pods) == 0 {
		return
	}
	if anyOwnedByStatefulSet(pods) {
		slices.SortStableFunc(pods, func(a, b *corev1.Pod) int {
			oa, aok := podOrdinal(a)
			ob, bok := podOrdinal(b)
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

// anyOwnedByStatefulSet reports whether any pod in the slice is owned by a
// StatefulSet; one is enough since the slice holds peers of one workload.
func anyOwnedByStatefulSet(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if IsOwnedByKind(pod.OwnerReferences, "StatefulSet") {
			return true
		}
	}
	return false
}

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

// podIsStale reports whether any container differs from its recommendation.
// Only sidecar init containers count; classic ones have already exited.
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
// (restartPolicy=Always).
func isRestartableInitContainer(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// applyRecsFiltered copies the containers and applies recommendations to
// those the predicate accepts (nil accepts all). Returns the copy and whether
// anything changed.
func applyRecsFiltered(in []corev1.Container, recs map[string]ContainerRecommendation, accept func(corev1.Container) bool) ([]corev1.Container, bool) {
	if len(in) == 0 {
		return in, false
	}
	// Deep copy: a shallow copy shares the ResourceList maps and would mutate
	// the caller's pod spec.
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

// setQuantity sets list[name] = *q, returning whether the value changed. A
// nil q is a no-op; an absent key compares as zero.
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

// applyLimit deletes the limit when remove is set, otherwise defers to
// setQuantity. remove wins over a non-nil q.
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
