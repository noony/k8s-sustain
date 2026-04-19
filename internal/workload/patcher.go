package workload

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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

// Patcher applies ContainerRecommendations to Kubernetes workloads.
//
// Ongoing mode behaviour:
//   - k8s ≥ 1.31 (inPlace=true): patches running pods directly via
//     InPlacePodVerticalScaling — zero restarts. Uses the /resize subresource
//     on k8s ≥ 1.33, falls back to a direct pod patch on 1.31-1.32.
//   - k8s < 1.31 (inPlace=false): evicts stale pods one by one via the
//     Eviction API so the workload controller replaces them from the updated
//     template. PodDisruptionBudgets are respected; pods blocked by a PDB are
//     skipped and retried on the next reconcile cycle.
type Patcher struct {
	client  client.Client
	inPlace bool
}

// New returns a Patcher. Set inPlace=true when the cluster supports
// InPlacePodVerticalScaling (k8s ≥ 1.31).
func New(c client.Client, inPlace bool) *Patcher {
	return &Patcher{client: c, inPlace: inPlace}
}

// PatchDeployment applies recs to deploy according to mode.
func (p *Patcher) PatchDeployment(ctx context.Context, deploy *appsv1.Deployment, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := deploy.DeepCopy()
	containers, changed := applyRecommendations(deploy.Spec.Template.Spec.Containers, recs)
	if !changed {
		return nil
	}
	deploy.Spec.Template.Spec.Containers = containers
	if err := p.client.Patch(ctx, deploy, client.MergeFrom(base)); err != nil {
		return err
	}
	if mode != sustainv1alpha1.UpdateModeOngoing {
		return nil
	}
	sel, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return fmt.Errorf("building pod selector for %s: %w", deploy.Name, err)
	}
	return p.recyclePods(ctx, deploy.Namespace, sel, recs)
}

// PatchStatefulSet applies recs to sts according to mode.
func (p *Patcher) PatchStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := sts.DeepCopy()
	containers, changed := applyRecommendations(sts.Spec.Template.Spec.Containers, recs)
	if !changed {
		return nil
	}
	sts.Spec.Template.Spec.Containers = containers
	if err := p.client.Patch(ctx, sts, client.MergeFrom(base)); err != nil {
		return err
	}
	if mode != sustainv1alpha1.UpdateModeOngoing {
		return nil
	}
	sel, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return fmt.Errorf("building pod selector for %s: %w", sts.Name, err)
	}
	return p.recyclePods(ctx, sts.Namespace, sel, recs)
}

// PatchDaemonSet applies recs to ds according to mode.
func (p *Patcher) PatchDaemonSet(ctx context.Context, ds *appsv1.DaemonSet, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := ds.DeepCopy()
	containers, changed := applyRecommendations(ds.Spec.Template.Spec.Containers, recs)
	if !changed {
		return nil
	}
	ds.Spec.Template.Spec.Containers = containers
	if err := p.client.Patch(ctx, ds, client.MergeFrom(base)); err != nil {
		return err
	}
	if mode != sustainv1alpha1.UpdateModeOngoing {
		return nil
	}
	sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return fmt.Errorf("building pod selector for %s: %w", ds.Name, err)
	}
	return p.recyclePods(ctx, ds.Namespace, sel, recs)
}

// PatchCronJob applies recs to the CronJob's job template so future runs use
// updated resources. No pod recycling needed — each scheduled run creates fresh pods.
func (p *Patcher) PatchCronJob(ctx context.Context, cj *batchv1.CronJob, recs map[string]ContainerRecommendation) error {
	base := cj.DeepCopy()
	containers, changed := applyRecommendations(
		cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
		recs,
	)
	if !changed {
		return nil
	}
	cj.Spec.JobTemplate.Spec.Template.Spec.Containers = containers
	return p.client.Patch(ctx, cj, client.MergeFrom(base))
}

// recyclePods drives running pods toward the updated resource spec.
// On clusters that support InPlacePodVerticalScaling the pod's resources are
// patched directly (zero restart). On older clusters each stale pod is evicted
// via the Eviction API so the workload controller replaces it from the updated
// template; PDB-blocked pods are skipped and retried on the next reconcile.
func (p *Patcher) recyclePods(ctx context.Context, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation) error {
	var podList corev1.PodList
	if err := p.client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	var errs []error
	for i := range podList.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		var err error
		if p.inPlace {
			err = p.patchPodInPlace(ctx, pod, recs)
		} else {
			err = p.evictPod(ctx, pod, recs)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
	}
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
	containers, changed := applyRecommendations(pod.Spec.Containers, recs)
	if !changed {
		return nil
	}
	pod.Spec.Containers = containers

	// K8s 1.33+ requires the /resize subresource for in-place pod resource
	// changes. Try that first; fall back to a regular pod patch for 1.31-1.32
	// where the subresource doesn't exist yet.
	err := p.client.SubResource("resize").Patch(ctx, pod, client.MergeFrom(base))
	if apierrors.IsNotFound(err) {
		logger.Info(err.Error())
		// /resize subresource not available (k8s < 1.33) — try direct pod patch.
		err = p.client.Patch(ctx, pod, client.MergeFrom(base))
	}
	if apierrors.IsInvalid(err) {
		// The API server rejected the pod resource patch — InPlacePodVerticalScaling
		// feature gate is not enabled on this cluster. Disable in-place for the rest
		// of this reconcile cycle and fall back to eviction.
		logger.Info("in-place pod resource patch rejected, feature gate likely disabled; falling back to eviction")
		p.inPlace = false
		pod.Spec.Containers = base.Spec.Containers
		return p.evictPod(ctx, pod, recs)
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
	if !podIsStale(pod, recs) {
		return nil // already running with the recommended resources
	}

	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	logger := log.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)

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
func podIsStale(pod *corev1.Pod, recs map[string]ContainerRecommendation) bool {
	for _, c := range pod.Spec.Containers {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		if rec.CPURequest != nil {
			current := c.Resources.Requests.Cpu()
			if current.Cmp(*rec.CPURequest) != 0 {
				return true
			}
		}
		if rec.MemoryRequest != nil {
			current := c.Resources.Requests.Memory()
			if current.Cmp(*rec.MemoryRequest) != 0 {
				return true
			}
		}
	}
	return false
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
