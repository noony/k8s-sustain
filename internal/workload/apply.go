package workload

import (
	"context"
	"errors"
	"fmt"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// ErrRefused is wrapped by every error Apply returns for a member it will not
// touch as it stands: a kind without an apply rule, an object of another
// kind, a member without a UID, a workload without a valid selector. Retrying
// cannot help until the member or the controller changes.
var ErrRefused = errors.New("refused")

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
	// Containers and InitContainers are what the member declares: its pod
	// template's, or a bare-pod identity's union. Apply narrows the
	// Recommendation to them.
	Containers, InitContainers []corev1.Container
}

// ApplySettings are the Policy's choices for applying to one member.
type ApplySettings struct {
	// RecommendOnly applies nothing and reads no pod.
	RecommendOnly bool
	// DryRun evaluates the member's pods without touching any, so OnCreate can
	// count the pods still waiting for a rollout to pick up the
	// Recommendation.
	DryRun bool
	// Tolerance withholds decreases below the Policy's downsize threshold.
	Tolerance Tolerance
	// IgnoreSafeToEvict evicts pods annotated safe-to-evict=false.
	IgnoreSafeToEvict bool
}

// ApplySettingsFor maps policy to its choices for a member of kind.
// recommendOnly is the controller's --recommend-only, which no Policy can
// turn off.
func ApplySettingsFor(policy *sustainv1alpha1.Policy, kind string, recommendOnly bool) ApplySettings {
	rs := policy.Spec.RightSizing
	mode := rs.Update.Types.ModeForKind(kind)
	return ApplySettings{
		RecommendOnly: policy.EffectiveRecommendOnly(recommendOnly),
		// Only Ongoing touches running pods; under OnCreate the webhook
		// applies resources at admission.
		DryRun:            mode == nil || *mode != sustainv1alpha1.UpdateModeOngoing,
		Tolerance:         toleranceOf(rs.ResourcesConfigs),
		IgnoreSafeToEvict: rs.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations,
	}
}

// ResultKind classifies what one Apply did with a member.
type ResultKind int

const (
	// ResultDone: the member's pods were driven toward the Recommendation,
	// and counted.
	ResultDone ResultKind = iota
	// ResultIdle: there was nothing to apply, no Recommendation for the
	// member's containers or recommend-only. No pod was read.
	ResultIdle
	// ResultCountedOnly: a dry run counted the pods still waiting for the
	// Recommendation, and touched none.
	ResultCountedOnly
	// ResultFailed: a step failed.
	ResultFailed
)

// Step is what applying to a member failed on: the reason its Blocked
// identity reports. Apply fails on StepPatch or StepResize; a caller that
// withholds the apply fails the member on StepPrometheus or StepStore.
type Step string

const (
	// StepPrometheus: the identity's inputs could not be read.
	StepPrometheus Step = "prometheus"
	// StepPatch: listing, resizing or evicting the pods of a kind that
	// evicts.
	StepPatch Step = "patch"
	// StepResize: listing or resizing the pods of an in-place-only kind.
	StepResize Step = "resize"
	// StepStore: the identity's Recommendation could not be stored for the
	// webhook, which the replacement of an evicted pod would read.
	StepStore Step = "store"
)

// Result is what one Apply did with a member.
type Result struct {
	Kind ResultKind
	// Step and Err are the step that failed and why, set for ResultFailed.
	Step Step
	Err  error
	// Pods counts the member's live pods and those still stale after the
	// pass, when Counted: always for ResultDone, and for ResultCountedOnly
	// unless the member's pods could not be listed.
	Pods    PodCounts
	Counted bool
	// Resized and Evicted name the pods resized in place and evicted.
	Resized, Evicted []string
	// Containers names, sorted, the containers whose resources changed on a
	// pod resized or evicted.
	Containers []string
	// Suppressed counts, per resource ("cpu", "memory"), the pods whose
	// decrease the Tolerance withheld.
	Suppressed map[string]int
}

// Changed counts the pods resized in place or evicted.
func (r Result) Changed() int { return len(r.Resized) + len(r.Evicted) }

func (r *Result) countSuppressed(resource string) {
	if r.Suppressed == nil {
		r.Suppressed = map[string]int{}
	}
	r.Suppressed[resource]++
}

// Apply drives a member's pods toward recs, the identity's Recommendation,
// narrowed to the containers the member declares. Pods are resized in place
// where the cluster supports it and otherwise evicted one at a time, so the
// webhook injects recs into the replacement. Pods of in-place-only kinds are
// never evicted.
func (p *Patcher) Apply(ctx context.Context, m Member, recs map[string]ContainerRecommendation, s ApplySettings) Result {
	logger := log.FromContext(ctx)
	recs = Inject(m.Containers, m.InitContainers, false, recs).Recs
	if len(recs) == 0 {
		logger.V(1).Info("no recommendation for the member's containers, nothing to apply")
		return Result{Kind: ResultIdle}
	}
	if s.RecommendOnly {
		logger.Info("recommend-only: applying nothing", "recommendations", recs)
		return Result{Kind: ResultIdle}
	}
	rule, ok := applyRules[m.Kind]
	if !ok {
		return Result{Kind: ResultFailed, Step: StepPatch, Err: fmt.Errorf("%w: no apply rule for kind %q", ErrRefused, m.Kind)}
	}
	step := StepPatch
	if rule.inPlaceOnly {
		step = StepResize
	}
	set, err := rule.podsOf(ctx, p.client, m)
	switch {
	case err != nil && s.DryRun:
		// Not a failure: a dry run touches no pod.
		logger.Info("counting the member's pods failed, pod counts left unchanged", "error", err.Error())
		return Result{Kind: ResultCountedOnly}
	case err != nil:
		return Result{Kind: ResultFailed, Step: step, Err: err}
	case s.DryRun:
		return Result{Kind: ResultCountedOnly, Pods: countPods(set.pods, recs, s.Tolerance, nil), Counted: true}
	}
	logger.V(1).Info("applying recommendations", "recommendations", recs)
	var res Result
	if rule.inPlaceOnly {
		res, err = p.resizeInPlaceOnly(ctx, set.pods, recs, s)
	} else {
		res, err = p.recycle(ctx, set, recs, s)
	}
	if err != nil {
		res.Kind, res.Step, res.Err = ResultFailed, step, err
		return res
	}
	res.Kind = ResultDone
	return res
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
		return zero, fmt.Errorf("%w: %s member is a %T", ErrRefused, m.Kind, m.Object)
	}
	if obj.GetUID() == "" {
		return obj, fmt.Errorf("%w: %s %s/%s has no UID", ErrRefused, m.Kind, obj.GetNamespace(), obj.GetName())
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
			return podSet{}, fmt.Errorf("%w: %s %s/%s has no selector", ErrRefused, m.Kind, obj.GetNamespace(), obj.GetName())
		}
		sel, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return podSet{}, fmt.Errorf("%w: parsing selector of %s %s/%s: %w", ErrRefused, m.Kind, obj.GetNamespace(), obj.GetName(), err)
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
// owner-name grouping them is an annotation, which no selector can match, and
// the inventory also leaves out pods the Policy does not govern.
func barePods(ctx context.Context, _ client.Client, m Member) (podSet, error) {
	pods := make([]*corev1.Pod, 0, len(m.Pods))
	for _, pod := range m.Pods {
		if pod.UID == "" {
			return podSet{}, fmt.Errorf("%w: bare pod %s/%s has no UID", ErrRefused, pod.Namespace, pod.Name)
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
