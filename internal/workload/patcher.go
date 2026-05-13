package workload

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
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

// Patcher recycles pods of Kubernetes workloads so they pick up the latest
// resource recommendations (injected by the admission webhook at pod creation).
//
// The Patcher never modifies workload specs (Deployment, StatefulSet, etc.).
//
// Ongoing mode behaviour:
//   - k8s ≥ 1.31 (inPlace=true): patches running pods directly via
//     InPlacePodVerticalScaling — zero restarts. Uses the /resize subresource
//     on k8s ≥ 1.33, falls back to a direct pod patch on 1.31-1.32.
//   - k8s < 1.31 (inPlace=false): evicts stale pods one by one via the
//     Eviction API so the workload controller replaces them. The webhook
//     injects the latest resources into the replacement pods.
//     PodDisruptionBudgets are respected; pods blocked by a PDB are
//     skipped and retried on the next reconcile cycle.
type Patcher struct {
	client client.Client
	// inPlace is read on every recycle and may be flipped to false from
	// any goroutine when the API server rejects an in-place patch. Using
	// an atomic avoids a data race when reconciles for different
	// workloads share this Patcher.
	inPlace atomic.Bool
}

// New returns a Patcher. Set inPlace=true when the cluster supports
// InPlacePodVerticalScaling (k8s ≥ 1.31).
func New(c client.Client, inPlace bool) *Patcher {
	p := &Patcher{client: c}
	p.inPlace.Store(inPlace)
	return p
}

// InPlace reports whether the patcher is currently using in-place pod
// resource updates. It can flip to false at runtime if the API server
// rejects an in-place patch (feature gate disabled).
func (p *Patcher) InPlace() bool { return p.inPlace.Load() }

// RecyclePods drives running pods matching the given selector toward the
// recommended resources. This is the only public entry point for pod recycling.
func (p *Patcher) RecyclePods(ctx context.Context, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation) error {
	return p.recyclePods(ctx, namespace, selector, recs)
}

// ResizePodsInPlace resizes the given pods in place, never falling back to
// eviction. It is the entry point for short-lived pods (Job, CronJob runs)
// where eviction would kill the work in progress: if the cluster doesn't
// support InPlacePodVerticalScaling, the resize is silently skipped and the
// new resources land on the next pod creation via the webhook.
//
// The caller is responsible for filtering out pods that should not be touched
// (e.g. pods owned by Completed/Failed Jobs).
func (p *Patcher) ResizePodsInPlace(ctx context.Context, pods []*corev1.Pod, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx)
	if !p.inPlace.Load() {
		logger.V(1).Info("in-place resize disabled on this cluster; deferring to next pod creation via webhook")
		return nil
	}

	var errs []error
	processed, skipped := 0, 0
	for _, pod := range pods {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping pod", "pod", pod.Name, "phase", pod.Status.Phase, "deleting", pod.DeletionTimestamp != nil)
			skipped++
			continue
		}
		if err := p.resizePodInPlaceNoEvict(ctx, pod, recs); err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		processed++
	}
	logger.Info("in-place resize pass complete", "processed", processed, "skipped", skipped, "errors", len(errs))
	return errors.Join(errs...)
}

// resizePodInPlaceNoEvict mirrors patchPodInPlace but never evicts. Infeasible
// resizes and feature-gate rejections are logged and skipped — the next pod
// creation will pick up the new resources via the webhook.
func (p *Patcher) resizePodInPlaceNoEvict(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	switch pod.Status.Resize {
	case corev1.PodResizeStatusInfeasible:
		logger.Info("in-place resize infeasible for short-lived pod, skipping (next run will pick up new resources)")
		return nil
	case corev1.PodResizeStatusDeferred:
		logger.Info("in-place resize deferred by kubelet, will apply when conditions allow")
		return nil
	}

	base := pod.DeepCopy()
	containers, regChanged := applyRecommendations(pod.Spec.Containers, recs)
	initContainers, initChanged := applyRecommendationsToSidecars(pod.Spec.InitContainers, recs)
	if !regChanged && !initChanged {
		logger.V(1).Info("pod already at target resources, no in-place patch needed")
		return nil
	}

	if regChanged {
		pod.Spec.Containers = containers
		err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(base))
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("/resize subresource not available, falling back to direct pod patch")
			err = p.client.Patch(ctx, pod, client.MergeFrom(base))
		}
		if apierrors.IsInvalid(err) {
			logger.Info("in-place pod resource patch rejected, feature gate likely disabled; skipping (next run via webhook)")
			p.inPlace.Store(false)
			pod.Spec.Containers = base.Spec.Containers
			return nil
		}
		if err != nil {
			return err
		}
		logger.Info("in-place resize applied")
	}

	if initChanged {
		sidecarBase := pod.DeepCopy()
		sidecarBase.Spec.InitContainers = base.Spec.InitContainers
		pod.Spec.InitContainers = initContainers

		err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(sidecarBase))
		if err != nil {
			logger.Info("sidecar in-place resize not accepted, will apply at next pod creation",
				"err", err.Error())
			pod.Spec.InitContainers = base.Spec.InitContainers
		} else {
			logger.Info("sidecar in-place resize applied")
		}
	}
	return nil
}

// recyclePods drives running pods toward the updated resource spec.
// On clusters that support InPlacePodVerticalScaling the pod's resources are
// patched directly (zero restart). On older clusters each stale pod is evicted
// via the Eviction API so the workload controller replaces it from the updated
// template; PDB-blocked pods are skipped and retried on the next reconcile.
func (p *Patcher) recyclePods(ctx context.Context, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("namespace", namespace, "selector", selector.String())

	var podList corev1.PodList
	if err := p.client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	strategy := "eviction"
	if p.inPlace.Load() {
		strategy = "inPlace"
	}
	logger.V(1).Info("listed pods for recycle", "count", len(podList.Items), "strategy", strategy)

	var errs []error
	processed, skipped := 0, 0
	for i := range podList.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pod := &podList.Items[i]
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
		if p.inPlace.Load() && pod.Status.Phase != corev1.PodRunning {
			logger.V(1).Info("skipping non-Running pod for in-place resize", "pod", pod.Name, "phase", pod.Status.Phase)
			skipped++
			continue
		}
		var err error
		if p.inPlace.Load() {
			err = p.patchPodInPlace(ctx, pod, recs)
		} else {
			err = p.evictPod(ctx, pod, recs)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
		processed++
	}
	logger.Info("recycle pass complete", "processed", processed, "skipped", skipped, "errors", len(errs), "strategy", strategy)
	return errors.Join(errs...)
}

// patchPodInPlace patches a single pod's container resources without restarting it.
//
// Before issuing the patch it checks pod.Status.Resize, which the kubelet
// populates after processing a previous in-place resize request:
//
//   - Infeasible: the node cannot satisfy the resources; fall back to eviction
//     so the scheduler can place the replacement pod elsewhere.
//   - Deferred: the kubelet accepted the request but is waiting for the right
//     conditions (e.g. a memory decrease that requires the container to restart).
//     Skip for now — the kubelet will apply it without further action from us.
//   - InProgress / "" (not set): patch is being applied or not yet requested;
//     proceed normally.
func (p *Patcher) patchPodInPlace(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	switch pod.Status.Resize {
	case corev1.PodResizeStatusInfeasible:
		logger.Info("in-place resize infeasible, falling back to eviction")
		return p.evictPod(ctx, pod, recs)
	case corev1.PodResizeStatusDeferred:
		logger.Info("in-place resize deferred by kubelet, will apply when conditions allow")
		return nil
	}

	base := pod.DeepCopy()
	containers, regChanged := applyRecommendations(pod.Spec.Containers, recs)
	initContainers, initChanged := applyRecommendationsToSidecars(pod.Spec.InitContainers, recs)
	if !regChanged && !initChanged {
		logger.V(1).Info("pod already at target resources, no in-place patch needed")
		return nil
	}

	// Patch regular containers and sidecar init containers in two separate
	// /resize calls. Sidecar in-place resize requires k8s 1.33+ with the right
	// feature gates and may be rejected on older clusters; isolating it from
	// the regular-container patch ensures a sidecar rejection cannot block the
	// regular containers' resize.
	if regChanged {
		pod.Spec.Containers = containers
		if err := p.applyInPlaceResize(ctx, pod, base, recs); err != nil {
			return err
		}
	}

	if initChanged {
		// Build a baseline that reflects the (possibly already applied)
		// regular-container changes so the sidecar diff is the only remaining
		// delta. The base in-place resize call above mutated `pod` in place.
		sidecarBase := pod.DeepCopy()
		sidecarBase.Spec.InitContainers = base.Spec.InitContainers
		pod.Spec.InitContainers = initContainers

		err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(sidecarBase))
		if err != nil {
			// Sidecar resize is best-effort: kubelet rejects it on older
			// clusters. The new requests will land at next pod creation via
			// webhook injection — don't fail the reconcile.
			logger.Info("sidecar in-place resize not accepted, will apply at next pod creation",
				"err", err.Error())
			pod.Spec.InitContainers = base.Spec.InitContainers
		} else {
			logger.Info("sidecar in-place resize applied")
		}
	}
	return nil
}

// applyInPlaceResize submits an in-place /resize patch (with fallbacks for
// older clusters) for the regular-container changes already staged on `pod`.
// Returns an error only when the pod could not be brought to its target state
// at all; eviction-fallback errors propagate to the caller as well.
func (p *Patcher) applyInPlaceResize(ctx context.Context, pod, base *corev1.Pod, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	// K8s 1.33+ requires the /resize subresource for in-place pod resource
	// changes. Try that first; fall back to a regular pod patch for 1.31-1.32
	// where the subresource doesn't exist yet.
	logger.V(1).Info("attempting in-place resize via /resize subresource")
	err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(base))
	if apierrors.IsNotFound(err) {
		logger.V(1).Info("/resize subresource not available, falling back to direct pod patch", "err", err.Error())
		err = p.client.Patch(ctx, pod, client.MergeFrom(base))
	}
	if apierrors.IsInvalid(err) {
		// The API server rejected the pod resource patch — InPlacePodVerticalScaling
		// feature gate is not enabled on this cluster. Disable in-place for the rest
		// of this reconcile cycle and fall back to eviction.
		logger.Info("in-place pod resource patch rejected, feature gate likely disabled; falling back to eviction")
		p.inPlace.Store(false)
		pod.Spec.Containers = base.Spec.Containers
		pod.Spec.InitContainers = base.Spec.InitContainers
		return p.evictPod(ctx, pod, recs)
	}
	if err == nil {
		logger.Info("in-place resize applied")
	}
	return err
}

// evictPod evicts a pod if it is running stale resources, so the workload
// controller replaces it from the updated template.
//
// A 429 (Too Many Requests) response from the Eviction API means a
// PodDisruptionBudget is blocking the eviction. The pod is skipped silently —
// it will be retried on the next reconcile cycle.
func (p *Patcher) evictPod(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

	if !podIsStale(pod, recs) {
		logger.V(1).Info("pod already running recommended resources, eviction skipped")
		return nil // already running with the recommended resources
	}

	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	logger.Info("evicting stale pod")
	err := p.client.SubResource("eviction").Create(ctx, pod, eviction)
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		logger.Info("pod already deleted, skipping eviction")
		return nil
	}
	if apierrors.IsTooManyRequests(err) {
		// PDB is blocking — log and move on; next reconcile will retry.
		logger.Info("eviction blocked by PodDisruptionBudget, will retry")
		return nil
	}
	return err
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
		if rec.CPURequest != nil && c.Resources.Requests.Cpu().Cmp(*rec.CPURequest) != 0 {
			return true
		}
		if rec.MemoryRequest != nil && c.Resources.Requests.Memory().Cmp(*rec.MemoryRequest) != 0 {
			return true
		}
		if limitStale(c.Resources.Limits.Cpu(), rec.CPULimit, rec.RemoveCPULimit) {
			return true
		}
		if limitStale(c.Resources.Limits.Memory(), rec.MemoryLimit, rec.RemoveMemoryLimit) {
			return true
		}
	}
	return false
}

// limitStale reports whether a container's current limit differs from the
// recommendation. A nil rec without remove=true means "leave it alone".
// Resources.Limits.Cpu()/Memory() return a zero-valued Quantity (never nil)
// when the limit is unset, which IsZero() detects.
func limitStale(current *resource.Quantity, rec *resource.Quantity, remove bool) bool {
	if remove {
		return !current.IsZero()
	}
	if rec == nil {
		return false
	}
	return current.Cmp(*rec) != 0
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

// applyRecommendationsToSidecars mirrors applyRecommendations but only mutates
// restartable init containers (sidecars). Classic init containers have already
// exited in a Running pod, so patching their resources in-place would be a
// no-op at best and an API error on some versions.
func applyRecommendationsToSidecars(in []corev1.Container, recs map[string]ContainerRecommendation) ([]corev1.Container, bool) {
	if len(in) == 0 {
		return in, false
	}
	out := make([]corev1.Container, len(in))
	copy(out, in)
	changed := false
	for i, c := range out {
		if !isRestartableInitContainer(c) {
			continue
		}
		if applyRecToContainer(&out[i], recs[c.Name]) {
			changed = true
		}
	}
	return out, changed
}

// applyRecToContainer applies a single recommendation to one container in
// place, returning whether anything changed. A nil/zero rec is a no-op.
func applyRecToContainer(c *corev1.Container, rec ContainerRecommendation) bool {
	if rec.CPURequest == nil && rec.MemoryRequest == nil &&
		rec.CPULimit == nil && rec.MemoryLimit == nil &&
		!rec.RemoveCPULimit && !rec.RemoveMemoryLimit {
		return false
	}
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	changed := false
	if rec.CPURequest != nil {
		c.Resources.Requests[corev1.ResourceCPU] = *rec.CPURequest
		changed = true
	}
	switch {
	case rec.RemoveCPULimit:
		delete(c.Resources.Limits, corev1.ResourceCPU)
		changed = true
	case rec.CPULimit != nil:
		c.Resources.Limits[corev1.ResourceCPU] = *rec.CPULimit
		changed = true
	}
	if rec.MemoryRequest != nil {
		c.Resources.Requests[corev1.ResourceMemory] = *rec.MemoryRequest
		changed = true
	}
	switch {
	case rec.RemoveMemoryLimit:
		delete(c.Resources.Limits, corev1.ResourceMemory)
		changed = true
	case rec.MemoryLimit != nil:
		c.Resources.Limits[corev1.ResourceMemory] = *rec.MemoryLimit
		changed = true
	}
	return changed
}

// applyRecommendations modifies container resources and returns
// (updated slice, whether any change was made).
func applyRecommendations(in []corev1.Container, recs map[string]ContainerRecommendation) ([]corev1.Container, bool) {
	out := make([]corev1.Container, len(in))
	copy(out, in)
	changed := false

	for i, c := range out {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}

		if out[i].Resources.Requests == nil {
			out[i].Resources.Requests = corev1.ResourceList{}
		}
		if out[i].Resources.Limits == nil {
			out[i].Resources.Limits = corev1.ResourceList{}
		}

		if rec.CPURequest != nil {
			out[i].Resources.Requests[corev1.ResourceCPU] = *rec.CPURequest
			changed = true
		}
		switch {
		case rec.RemoveCPULimit:
			delete(out[i].Resources.Limits, corev1.ResourceCPU)
			changed = true
		case rec.CPULimit != nil:
			out[i].Resources.Limits[corev1.ResourceCPU] = *rec.CPULimit
			changed = true
		}

		if rec.MemoryRequest != nil {
			out[i].Resources.Requests[corev1.ResourceMemory] = *rec.MemoryRequest
			changed = true
		}
		switch {
		case rec.RemoveMemoryLimit:
			delete(out[i].Resources.Limits, corev1.ResourceMemory)
			changed = true
		case rec.MemoryLimit != nil:
			out[i].Resources.Limits[corev1.ResourceMemory] = *rec.MemoryLimit
			changed = true
		}
	}
	return out, changed
}
