package workload

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
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
		if !fixed[pod.UID] && planFor(pod, recs, tol).stale() {
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

// changes collects what one pass did to a member's pods.
type changes struct {
	res        Result
	fixed      map[types.UID]bool
	containers map[string]bool
}

func newChanges() *changes {
	return &changes{fixed: map[types.UID]bool{}, containers: map[string]bool{}}
}

// resized records an in-place resize of pod that changed containers.
func (c *changes) resized(pod *corev1.Pod, containers []string) {
	c.res.Resized = append(c.res.Resized, pod.Name)
	c.fixed[pod.UID] = true
	for _, name := range containers {
		c.containers[name] = true
	}
}

// evicted records the eviction of pod, whose replacement the webhook gives
// recs: a container changes where recs changes the resources it runs with.
func (c *changes) evicted(pod *corev1.Pod, recs map[string]ContainerRecommendation) {
	c.res.Evicted = append(c.res.Evicted, pod.Name)
	c.fixed[pod.UID] = true
	containers, initContainers := running(pod)
	for _, w := range Inject(containers, initContainers, false, recs).Writes {
		c.containers[w.Name] = true
	}
}

// result is the pass's Result, counted against pods unless it failed.
func (c *changes) result(pods []*corev1.Pod, recs map[string]ContainerRecommendation, tol Tolerance, err error) (Result, error) {
	c.res.Containers = slices.Sorted(maps.Keys(c.containers))
	if err == nil {
		c.res.Pods, c.res.Counted = countPods(pods, recs, tol, c.fixed), true
	}
	return c.res, err
}

// resizeInPlaceOnly resizes running pods in place and never evicts, for kinds
// where eviction would destroy work nothing redoes. Without in-place support
// it only counts.
func (p *Patcher) resizeInPlaceOnly(ctx context.Context, pods []*corev1.Pod, recs map[string]ContainerRecommendation, s ApplySettings) (Result, error) {
	c := newChanges()
	logger := log.FromContext(ctx)
	if !p.inPlace {
		logger.V(1).Info("in-place resize disabled on this cluster; deferring to next pod creation via webhook")
		return c.result(pods, recs, s.Tolerance, nil)
	}

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return c.result(pods, recs, s.Tolerance, ctx.Err())
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping pod", "pod", pod.Name, "phase", pod.Status.Phase, "deleting", pod.DeletionTimestamp != nil)
			skipped++
			continue
		}
		plan := planFor(pod, recs, s.Tolerance)
		observeSuppressed(recs, plan.recs, c.res.countSuppressed)
		written, err := p.resizePodInPlaceNoEvict(ctx, pod, plan.recs)
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		if len(written) > 0 {
			c.resized(pod, written)
		}
		processed++
	}
	logger.Info("in-place resize pass complete", "processed", processed, "skipped", skipped, "resized", len(c.res.Resized), "errors", len(errs))
	return c.result(pods, recs, s.Tolerance, errors.Join(errs...))
}

// unapplyStrategy decides what happens when an in-place resize cannot be
// applied. Both hooks return (evicted, err).
type unapplyStrategy struct {
	unappliedLog    string
	onUnsatisfiable func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error)
	onUnapplied     func(ctx context.Context, pod *corev1.Pod) (bool, error)
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
// in-place-only kinds and for a change to limits alone: a pod it cannot
// resize keeps its resources until it is replaced.
func (p *Patcher) resizePodInPlaceNoEvict(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) ([]string, error) {
	written, _, err := p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unappliedLog: "in-place resize not applied, the pod keeps its resources until it is replaced",
		onUnsatisfiable: func(context.Context, *corev1.Pod, string) (bool, error) {
			return false, nil
		},
		onUnapplied: func(context.Context, *corev1.Pod) (bool, error) {
			return false, nil
		},
	})
	return written, err
}

// resizePodInPlaceWith is the in-place resize body shared by patchPodInPlace
// and resizePodInPlaceNoEvict. It returns the containers the resizes it
// submitted changed, whether it evicted the pod instead, and the error.
//
// The kubelet's verdict is only consulted when the spec already carries the
// target: a verdict refers to the staged resize, so a changed recommendation
// is submitted and re-evaluated.
func (p *Patcher) resizePodInPlaceWith(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, strat unapplyStrategy) (written []string, evicted bool, err error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	base := pod.DeepCopy()
	r := resized(logger, pod, recs)
	if len(r.regular) == 0 && len(r.sidecars) == 0 {
		switch verdict := resizePendingReason(pod); verdict {
		case corev1.PodReasonInfeasible, corev1.PodReasonError:
			logger.Info("staged in-place resize cannot complete", "verdict", verdict)
			evicted, err = strat.onUnsatisfiable(ctx, pod, verdict)
			return nil, evicted, err
		case corev1.PodReasonDeferred:
			logger.Info("in-place resize deferred by kubelet, will apply when conditions allow")
			return nil, false, nil
		case "":
			logger.V(1).Info("pod already at target resources, no in-place patch needed")
			return nil, false, nil
		default:
			logger.Info("unrecognized resize verdict, leaving pod to the kubelet", "verdict", verdict)
			return nil, false, nil
		}
	}

	// Sidecars are resized in a separate /resize call: they need extra feature
	// gates and a rejection must not block the regular containers.
	if len(r.regular) > 0 {
		pod.Spec.Containers = r.containers
		switch answer, err := p.submitInPlaceResize(ctx, pod, base); {
		case err != nil:
			return nil, false, err
		case answer == resizeGone:
			return nil, false, nil
		case answer == resizeInvalid:
			logger.Info(strat.unappliedLog)
			evicted, err = strat.onUnapplied(ctx, pod)
			return nil, evicted, err
		}
		written = r.regular
	}

	if len(r.sidecars) > 0 && p.applySidecarResize(ctx, pod, base, r.initContainers) {
		written = append(written, r.sidecars...)
	}
	return written, false, nil
}

// recycle resizes or evicts stale pods one at a time, waiting for each
// replacement and aborting on CrashLoopBackOff so a bad recommendation
// cannot cascade through the workload, within a pass or across passes.
func (p *Patcher) recycle(ctx context.Context, set podSet, recs map[string]ContainerRecommendation, s ApplySettings) (Result, error) {
	c := newChanges()
	logger := log.FromContext(ctx).WithValues("namespace", set.namespace, "selector", set.selector.String())
	strategy := "eviction"
	if p.inPlace {
		strategy = "inPlace"
	}
	logger.V(1).Info("listed pods for recycle", "count", len(set.pods), "strategy", strategy)

	pods := set.pods
	sortPodsForRecycle(pods)
	// Read before the loop: an in-place resize rewrites the spec of the pod it
	// resizes, and a stale pod resized this pass is no proof against the
	// recommendation.
	gate := evictionGate{ignoreSafeToEvict: s.IgnoreSafeToEvict, halted: updatedPodCrashLooping(pods, recs, s.Tolerance)}

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return c.result(pods, recs, s.Tolerance, ctx.Err())
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
		// Plan per pod: siblings may carry different current allocations.
		plan := planFor(pod, recs, s.Tolerance)
		observeSuppressed(recs, plan.recs, c.res.countSuppressed)
		var (
			written []string
			evicted bool
			err     error
		)
		switch inPlace := p.inPlace && pod.Status.Phase == corev1.PodRunning; {
		case inPlace && plan.change == limitChange:
			written, err = p.resizePodInPlaceNoEvict(ctx, pod, plan.recs)
		case inPlace:
			written, evicted, err = p.patchPodInPlace(ctx, pod, plan.recs, gate)
		// A pod that is not Running cannot be resized in place, so a stale one
		// is evicted in both modes: one stuck Pending on an oversized request
		// is exactly what the webhook should re-inject.
		case plan.stale():
			evicted, err = p.submitEviction(ctx, pod, "stale resources", gate)
		case plan.change == limitChange:
			logger.V(1).Info("only limits differ, left to the pod's replacement", "pod", pod.Name)
		default:
			logger.V(1).Info("pod already running recommended resources, eviction skipped", "pod", pod.Name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		if errors.Is(err, errCrashLoopBackOff) {
			logger.Info("halting eviction loop for this reconcile", "reason", err.Error())
			break
		}
		processed++
		if len(written) > 0 {
			c.resized(pod, written)
		}
		if !evicted {
			continue
		}
		c.evicted(pod, recs)
		if waitErr := p.waitForReplacement(ctx, set.namespace, set.selector, pod.Name, pod.UID); waitErr != nil {
			errs = append(errs, fmt.Errorf("after evicting %s: %w", pod.Name, waitErr))
			logger.Info("halting eviction loop for this reconcile", "reason", waitErr.Error())
			break
		}
	}
	logger.Info("recycle pass complete", "processed", processed, "skipped", skipped,
		"resized", len(c.res.Resized), "evicted", len(c.res.Evicted), "errors", len(errs), "strategy", strategy)
	return c.result(pods, recs, s.Tolerance, errors.Join(errs...))
}

// patchPodInPlace resizes a pod whose requests change in place, falling back
// to eviction when the resize is rejected as Invalid, or when the kubelet
// reports a staged resize of a request Infeasible or Error. It returns what
// resizePodInPlaceWith does.
func (p *Patcher) patchPodInPlace(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation, gate evictionGate) ([]string, bool, error) {
	return p.resizePodInPlaceWith(ctx, pod, recs, unapplyStrategy{
		unappliedLog: "falling back to eviction",
		// The spec already matches the recommendation: only the resources
		// the kubelet failed to enact tell whether a request is at stake.
		onUnsatisfiable: func(ctx context.Context, pod *corev1.Pod, verdict string) (bool, error) {
			if !stagedRequests(pod) {
				log.FromContext(ctx).Info("the staged resize changes limits alone, left to the pod's replacement",
					"pod", pod.Name, "namespace", pod.Namespace, "verdict", verdict)
				return false, nil
			}
			return p.submitEviction(ctx, pod, "in-place resize verdict "+verdict, gate)
		},
		onUnapplied: func(ctx context.Context, pod *corev1.Pod) (bool, error) {
			return p.submitEviction(ctx, pod, "stale resources", gate)
		},
	})
}

// stagedRequests reports whether the resize staged on pod changes a request:
// the kubelet reports, for a regular or sidecar container, requests other
// than its spec's, or none at all, which may hide one.
func stagedRequests(pod *corev1.Pod) bool {
	enacted := enactedResources(pod)
	staged := func(c corev1.Container) bool {
		r, ok := enacted[c.Name]
		return !ok || !requestsEqual(c.Resources, r)
	}
	return slices.ContainsFunc(pod.Spec.Containers, staged) ||
		slices.ContainsFunc(pod.Spec.InitContainers, func(c corev1.Container) bool {
			return isRestartableInitContainer(c) && staged(c)
		})
}

// resizeAnswer is how the apiserver answered a submitted resize.
type resizeAnswer int

const (
	resizeAccepted resizeAnswer = iota
	// resizeInvalid: rejected for this pod, which is reverted to base.
	resizeInvalid
	// resizeGone: the pod no longer exists.
	resizeGone
	// resizeFailed: the request failed otherwise; the error says why.
	resizeFailed
)

// submitInPlaceResize patches the /resize subresource with the diff from
// base to pod.
func (p *Patcher) submitInPlaceResize(ctx context.Context, pod, base *corev1.Pod) (resizeAnswer, error) {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	logger.V(1).Info("attempting in-place resize via /resize subresource")
	err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(base))
	switch {
	case err == nil:
		logger.Info("in-place resize applied")
		return resizeAccepted, nil
	case apierrors.IsInvalid(err):
		logger.Info("in-place resize rejected as invalid for this pod (e.g. QoS class change)", "err", err.Error())
		revertStagedResize(pod, base)
		return resizeInvalid, nil
	case apierrors.IsNotFound(err):
		logger.Info("pod gone before in-place resize, skipping")
		return resizeGone, nil
	default:
		return resizeFailed, err
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

// submitEviction creates the Eviction without a staleness gate: the caller
// decided the pod's plan allows one. It is the single gate check for every
// eviction trigger, and returns false for a pod gone, PDB-blocked or
// annotated safe-to-evict=false.
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
		if planFor(pod, recs, tol).stale() {
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

// isRestartableInitContainer reports whether an init container is a sidecar
// (restartPolicy=Always).
func isRestartableInitContainer(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// resize is a pod's containers and init containers with a Recommendation
// applied, and the names of the regular and sidecar containers that changed.
type resize struct {
	containers, initContainers []corev1.Container
	regular, sidecars          []string
}

// resized applies recs to the pod's containers and init containers. Of the
// init containers only sidecars are resized: a classic one has already
// exited.
func resized(logger logr.Logger, pod *corev1.Pod, recs map[string]ContainerRecommendation) resize {
	r := resize{containers: slices.Clone(pod.Spec.Containers), initContainers: slices.Clone(pod.Spec.InitContainers)}
	for _, w := range Inject(pod.Spec.Containers, pod.Spec.InitContainers, false, recs).Writes {
		switch {
		case !w.Init:
			r.containers[w.Index].Resources = w.Resources
			r.regular = append(r.regular, w.Name)
		case isRestartableInitContainer(r.initContainers[w.Index]):
			r.initContainers[w.Index].Resources = w.Resources
			r.sidecars = append(r.sidecars, w.Name)
		default:
			continue
		}
		w.LogRaisedLimits(logger)
	}
	return r
}

// running returns the pod's containers and init containers with the
// resources they run with: those the kubelet reports it enacted, or else the
// spec's.
func running(pod *corev1.Pod) (containers, initContainers []corev1.Container) {
	enacted := enactedResources(pod)
	withEnacted := func(cs []corev1.Container) []corev1.Container {
		out := slices.Clone(cs)
		for i := range out {
			if r, ok := enacted[out[i].Name]; ok {
				out[i].Resources = r
			}
		}
		return out
	}
	return withEnacted(pod.Spec.Containers), withEnacted(pod.Spec.InitContainers)
}

// enactedResources maps each container, regular or init, whose status reports
// them to the resources the kubelet enacted, which lag the spec while a resize
// is staged.
func enactedResources(pod *corev1.Pod) map[string]corev1.ResourceRequirements {
	out := map[string]corev1.ResourceRequirements{}
	for _, s := range slices.Concat(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses) {
		if s.Resources != nil {
			out[s.Name] = *s.Resources
		}
	}
	return out
}
