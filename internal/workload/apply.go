package workload

import (
	"context"
	"fmt"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Member is one live object of an identity that a Recommendation is applied
// to. For a workload kind it is the workload object, and only pods whose
// controller ownerRef chain resolves to its UID are touched. A bare-pod
// identity has no owner object: its governed pods are applied together, each
// its own member object.
type Member struct {
	Kind string
	// Object is the workload object. Not read for kind Pod.
	Object client.Object
	// Pods are a bare-pod identity's governed pods. Read only for kind Pod.
	Pods []*corev1.Pod
}

// ApplySettings are the Policy's choices for one Apply.
type ApplySettings struct {
	// DryRun evaluates the member's pods without touching any, so OnCreate can
	// count the pods still waiting for a rollout to pick up the
	// Recommendation.
	DryRun bool
	// Tolerance withholds decreases below the Policy's downsize threshold.
	Tolerance Tolerance
	// IgnoreSafeToEvict evicts pods annotated safe-to-evict=false.
	IgnoreSafeToEvict bool
}

// Outcome is what one Apply did to a member's pods.
type Outcome struct {
	// InPlaceOnly is set, error or not, for kinds whose pods are never
	// evicted.
	InPlaceOnly bool
	// Changed counts the pods resized in place or evicted.
	Changed int
	// Pods counts the member's live pods and those still stale after the
	// pass. Set only when Apply returns no error.
	Pods PodCounts
	// Suppressed counts, per resource ("cpu", "memory"), the pods whose
	// decrease the Tolerance withheld.
	Suppressed map[string]int
}

func (o *Outcome) countSuppressed(resource string) {
	if o.Suppressed == nil {
		o.Suppressed = map[string]int{}
	}
	o.Suppressed[resource]++
}

// Apply drives a member's pods toward recs, the Recommendation narrowed to
// the containers the member declares. Pods are resized in place where the
// cluster supports it and otherwise evicted one at a time, so the webhook
// injects recs into the replacement. Pods of in-place-only kinds are never
// evicted.
func (p *Patcher) Apply(ctx context.Context, m Member, recs map[string]ContainerRecommendation, s ApplySettings) (Outcome, error) {
	rule, ok := applyRules[m.Kind]
	if !ok {
		return Outcome{}, fmt.Errorf("no apply rule for kind %q", m.Kind)
	}
	set, err := rule.podsOf(ctx, p.client, m)
	if err != nil {
		return Outcome{InPlaceOnly: rule.inPlaceOnly}, err
	}
	if rule.inPlaceOnly {
		return p.resizeInPlaceOnly(ctx, set.pods, recs, s)
	}
	return p.recycle(ctx, set, recs, s)
}

// applyRule is how Apply treats one kind's members.
type applyRule struct {
	// inPlaceOnly kinds never have a pod evicted: eviction would destroy work
	// nothing redoes. Their running pods are resized in place or left alone.
	inPlaceOnly bool
	podsOf      func(ctx context.Context, c client.Client, m Member) (podSet, error)
}

// podSet is a member's pods as one pass found them.
type podSet struct {
	pods []*corev1.Pod
	// namespace and selector find the member's pods again while an evicted
	// pod's replacement is awaited. Unset for in-place-only kinds.
	namespace string
	selector  klabels.Selector
}

// applyRules holds one rule per kind in SupportedKinds.
var applyRules = map[string]applyRule{
	"Deployment":  {podsOf: selectedPods(func(o *appsv1.Deployment) *metav1.LabelSelector { return o.Spec.Selector })},
	"StatefulSet": {podsOf: selectedPods(func(o *appsv1.StatefulSet) *metav1.LabelSelector { return o.Spec.Selector })},
	"DaemonSet":   {podsOf: selectedPods(func(o *appsv1.DaemonSet) *metav1.LabelSelector { return o.Spec.Selector })},
	"Rollout":     {podsOf: selectedPods(func(o *rolloutsv1alpha1.Rollout) *metav1.LabelSelector { return o.Spec.Selector })},
	// Evicting a job pod kills the run; a CronJob's next run gets the
	// Recommendation from the webhook.
	"CronJob": {inPlaceOnly: true, podsOf: cronJobPods},
	// A standalone Job has no next run: resizing its running pod is the only
	// correction after creation.
	"Job": {inPlaceOnly: true, podsOf: jobPods},
	// No controller would recreate an evicted bare pod.
	"Pod": {inPlaceOnly: true, podsOf: barePods},
}

// memberObject returns the member's workload object as T. A member without a
// UID is refused: the UID is what tells its pods from a bystander's.
func memberObject[T client.Object](m Member) (T, error) {
	obj, ok := m.Object.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("%s member is a %T", m.Kind, m.Object)
	}
	if obj.GetUID() == "" {
		return obj, fmt.Errorf("%s %s/%s has no UID", m.Kind, obj.GetNamespace(), obj.GetName())
	}
	return obj, nil
}

// selectedPods lists the pods matching a workload's selector and keeps those
// it owns, so a bystander that merely shares the labels is never touched.
func selectedPods[T client.Object](selectorOf func(T) *metav1.LabelSelector) func(context.Context, client.Client, Member) (podSet, error) {
	return func(ctx context.Context, c client.Client, m Member) (podSet, error) {
		obj, err := memberObject[T](m)
		if err != nil {
			return podSet{}, err
		}
		// LabelSelectorAsSelector(nil) matches nothing: the member would look
		// converged while none of its pods was ever considered.
		ls := selectorOf(obj)
		if ls == nil {
			return podSet{}, fmt.Errorf("%s %s/%s has no selector", m.Kind, obj.GetNamespace(), obj.GetName())
		}
		sel, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return podSet{}, fmt.Errorf("parsing selector of %s %s/%s: %w", m.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
		var list corev1.PodList
		if err := c.List(ctx, &list, client.InNamespace(obj.GetNamespace()), client.MatchingLabelsSelector{Selector: sel}); err != nil {
			return podSet{}, fmt.Errorf("listing pods: %w", err)
		}
		logger := log.FromContext(ctx)
		rsOwned := map[string]bool{}
		pods := make([]*corev1.Pod, 0, len(list.Items))
		for i := range list.Items {
			pod := &list.Items[i]
			owned, err := PodOwnedByWorkload(ctx, c, pod, obj.GetUID(), rsOwned)
			if err != nil {
				return podSet{}, fmt.Errorf("resolving owner of pod %s: %w", pod.Name, err)
			}
			if !owned {
				logger.V(1).Info("skipping pod matching selector but not owned by target workload",
					"pod", pod.Name, "targetKind", m.Kind, "targetName", obj.GetName())
				continue
			}
			pods = append(pods, pod)
		}
		return podSet{pods: pods, namespace: obj.GetNamespace(), selector: sel}, nil
	}
}

func cronJobPods(ctx context.Context, c client.Client, m Member) (podSet, error) {
	cj, err := memberObject[*batchv1.CronJob](m)
	if err != nil {
		return podSet{}, err
	}
	jobs, err := activeJobsOf(ctx, c, cj)
	if err != nil {
		return podSet{}, fmt.Errorf("listing jobs for cronjob: %w", err)
	}
	var pods []*corev1.Pod
	for i := range jobs {
		jobPods, err := podsOfJob(ctx, c, &jobs[i])
		if err != nil {
			return podSet{}, fmt.Errorf("listing pods for job %s: %w", jobs[i].Name, err)
		}
		pods = append(pods, jobPods...)
	}
	log.FromContext(ctx).V(1).Info("listed cronjob pods", "cronjob", cj.Name, "jobs", len(jobs), "pods", len(pods))
	return podSet{pods: pods}, nil
}

func jobPods(ctx context.Context, c client.Client, m Member) (podSet, error) {
	job, err := memberObject[*batchv1.Job](m)
	if err != nil {
		return podSet{}, err
	}
	pods, err := podsOfJob(ctx, c, job)
	if err != nil {
		return podSet{}, fmt.Errorf("listing pods for job %s: %w", job.Name, err)
	}
	return podSet{pods: pods}, nil
}

// barePods takes the pods the inventory found rather than a selector: the
// mirrored owner-name label exists only on pods the webhook admitted, and the
// inventory also leaves out pods the Policy does not govern.
func barePods(ctx context.Context, _ client.Client, m Member) (podSet, error) {
	pods := make([]*corev1.Pod, 0, len(m.Pods))
	for _, pod := range m.Pods {
		if pod.UID == "" {
			return podSet{}, fmt.Errorf("bare pod %s/%s has no UID", pod.Namespace, pod.Name)
		}
		if ref := metav1.GetControllerOf(pod); ref != nil {
			log.FromContext(ctx).Info("skipping controller-owned pod in a bare-pod identity",
				"pod", pod.Name, "namespace", pod.Namespace, "ownerKind", ref.Kind, "ownerName", ref.Name)
			continue
		}
		pods = append(pods, pod)
	}
	return podSet{pods: pods}, nil
}
