package workload

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// When inPlace is true and the cluster supports InPlacePodVerticalScaling
// (alpha ≥ 1.27, beta default-on ≥ 1.29), Ongoing-mode updates also patch
// running Pods directly so they pick up new resource values without a restart.
// When inPlace is false (or mode is OnCreate), the traditional rollout-restart
// annotation is used instead.
type Patcher struct {
	client  client.Client
	inPlace bool
}

// New returns a Patcher. Set inPlace=true when the cluster supports
// InPlacePodVerticalScaling (k8s ≥ 1.27).
func New(c client.Client, inPlace bool) *Patcher {
	return &Patcher{client: c, inPlace: inPlace}
}

// PatchDeployment applies recs to deploy according to mode.
func (p *Patcher) PatchDeployment(ctx context.Context, deploy *appsv1.Deployment, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := deploy.DeepCopy()
	containers, changed := applyRecommendations(deploy.Spec.Template.Spec.Containers, mode, recs)
	if !changed {
		return nil
	}
	deploy.Spec.Template.Spec.Containers = containers
	if !p.inPlace {
		addRestartAnnotation(&deploy.Spec.Template, mode)
	}
	if err := p.client.Patch(ctx, deploy, client.MergeFrom(base)); err != nil {
		return err
	}
	if p.inPlace && mode == sustainv1alpha1.UpdateModeOngoing {
		sel, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
		if err != nil {
			return fmt.Errorf("building pod selector for %s: %w", deploy.Name, err)
		}
		return p.patchPodsInPlace(ctx, deploy.Namespace, sel, recs)
	}
	return nil
}

// PatchStatefulSet applies recs to sts according to mode.
func (p *Patcher) PatchStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := sts.DeepCopy()
	containers, changed := applyRecommendations(sts.Spec.Template.Spec.Containers, mode, recs)
	if !changed {
		return nil
	}
	sts.Spec.Template.Spec.Containers = containers
	if !p.inPlace {
		addRestartAnnotation(&sts.Spec.Template, mode)
	}
	if err := p.client.Patch(ctx, sts, client.MergeFrom(base)); err != nil {
		return err
	}
	if p.inPlace && mode == sustainv1alpha1.UpdateModeOngoing {
		sel, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
		if err != nil {
			return fmt.Errorf("building pod selector for %s: %w", sts.Name, err)
		}
		return p.patchPodsInPlace(ctx, sts.Namespace, sel, recs)
	}
	return nil
}

// PatchDaemonSet applies recs to ds according to mode.
func (p *Patcher) PatchDaemonSet(ctx context.Context, ds *appsv1.DaemonSet, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) error {
	base := ds.DeepCopy()
	containers, changed := applyRecommendations(ds.Spec.Template.Spec.Containers, mode, recs)
	if !changed {
		return nil
	}
	ds.Spec.Template.Spec.Containers = containers
	if !p.inPlace {
		addRestartAnnotation(&ds.Spec.Template, mode)
	}
	if err := p.client.Patch(ctx, ds, client.MergeFrom(base)); err != nil {
		return err
	}
	if p.inPlace && mode == sustainv1alpha1.UpdateModeOngoing {
		sel, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
		if err != nil {
			return fmt.Errorf("building pod selector for %s: %w", ds.Name, err)
		}
		return p.patchPodsInPlace(ctx, ds.Namespace, sel, recs)
	}
	return nil
}

// PatchCronJob applies recs to the CronJob's job template so future runs use
// updated resources. No restart annotation is added — each job run creates
// fresh pods, so the template change takes effect on the next schedule tick.
// In-place pod patching is not applicable to CronJob-spawned pods (ephemeral).
func (p *Patcher) PatchCronJob(ctx context.Context, cj *batchv1.CronJob, recs map[string]ContainerRecommendation) error {
	base := cj.DeepCopy()
	containers, changed := applyRecommendations(
		cj.Spec.JobTemplate.Spec.Template.Spec.Containers,
		sustainv1alpha1.UpdateModeOngoing,
		recs,
	)
	if !changed {
		return nil
	}
	cj.Spec.JobTemplate.Spec.Template.Spec.Containers = containers
	return p.client.Patch(ctx, cj, client.MergeFrom(base))
}

// patchPodsInPlace lists running, non-terminating Pods matched by selector and
// patches their container resources directly using the InPlacePodVerticalScaling
// API. Errors per Pod are collected and joined so all Pods are attempted.
func (p *Patcher) patchPodsInPlace(ctx context.Context, namespace string, selector klabels.Selector, recs map[string]ContainerRecommendation) error {
	var podList corev1.PodList
	if err := p.client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return fmt.Errorf("listing pods for in-place update: %w", err)
	}

	var errs []error
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if err := p.patchPodInPlace(ctx, pod, recs); err != nil {
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
	}
	return errors.Join(errs...)
}

// patchPodInPlace patches a single Pod's container resources in-place.
func (p *Patcher) patchPodInPlace(ctx context.Context, pod *corev1.Pod, recs map[string]ContainerRecommendation) error {
	base := pod.DeepCopy()
	// Reuse applyRecommendations with Ongoing mode (no OnCreate guard needed for direct pod patching).
	containers, changed := applyRecommendations(pod.Spec.Containers, sustainv1alpha1.UpdateModeOngoing, recs)
	if !changed {
		return nil
	}
	pod.Spec.Containers = containers
	return p.client.Patch(ctx, pod, client.MergeFrom(base))
}

// applyRecommendations modifies container resources following mode rules and returns
// (updated slice, whether any change was made).
// OnCreate: only sets resources on containers that have no CPU request yet.
// Ongoing:  always applies.
func applyRecommendations(in []corev1.Container, mode sustainv1alpha1.UpdateMode, recs map[string]ContainerRecommendation) ([]corev1.Container, bool) {
	out := make([]corev1.Container, len(in))
	copy(out, in)
	changed := false

	for i, c := range out {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		if mode == sustainv1alpha1.UpdateModeOnCreate {
			if c.Resources.Requests != nil && !c.Resources.Requests.Cpu().IsZero() {
				continue
			}
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

// addRestartAnnotation adds the kubectl rollout-restart annotation when mode is
// Ongoing, triggering a rolling restart so pods pick up new resource values.
func addRestartAnnotation(tmpl *corev1.PodTemplateSpec, mode sustainv1alpha1.UpdateMode) {
	if mode != sustainv1alpha1.UpdateModeOngoing {
		return
	}
	if tmpl.Annotations == nil {
		tmpl.Annotations = make(map[string]string)
	}
	tmpl.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().UTC().Format(time.RFC3339)
}
