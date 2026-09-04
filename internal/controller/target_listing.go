package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// collectTargets lists workloads of all enabled kinds and returns matching targets.
func (r *PolicyReconciler) collectTargets(ctx context.Context, policy *sustainv1alpha1.Policy) ([]workloadTarget, error) {
	logger := log.FromContext(ctx).WithValues("policy", policy.Name)
	types := policy.Spec.RightSizing.Update.Types
	namespaces := policy.Spec.Selector.Namespaces
	var targets []workloadTarget

	logger.V(1).Info("collecting targets",
		"namespaces", namespaces,
		"deployment", types.Deployment,
		"statefulset", types.StatefulSet,
		"daemonset", types.DaemonSet,
		"argoRollout", types.ArgoRollout,
		"cronjob", types.CronJob,
		"job", types.Job,
		"pod", types.Pod)

	nsAnn := newNSAnnotations(r.Client)
	for _, kind := range listedKinds {
		mode := types.ModeForKind(kind)
		if mode == nil {
			continue
		}
		var (
			t   []workloadTarget
			err error
		)
		if kind == "Pod" {
			t, err = r.listBarePodTargets(ctx, namespaces, nsAnn)
		} else {
			t, err = r.listTargetsOfKind(ctx, kind, namespaces)
		}
		if err != nil {
			return nil, fmt.Errorf("listing %ss: %w", strings.ToLower(kind), err)
		}
		for i := range t {
			t[i].UpdateMode = *mode
		}
		logger.V(1).Info("listed workloads", "kind", kind, "count", len(t))
		targets = append(targets, t...)
	}

	// Resolve each target's opt-in across all three annotation levels. Bare-pod
	// targets already carry a resolved PolicyName (GroupBarePods does the walk
	// as it groups, because the policy is part of the grouping rule itself).
	//
	// The Namespace read is LAZY: only issued when neither the pod template nor
	// the workload's own annotations already decide the outcome
	// (policymatch.DecidesAt). Both of those are already in memory from the
	// List that built targets, and reading the Namespace unconditionally would
	// let an RBAC gap or apiserver hiccup abort the whole reconcile for
	// workloads whose own annotation already answered the question.
	for i := range targets {
		t := &targets[i]
		if t.Kind == "Pod" {
			continue
		}
		var level policymatch.Level
		if policymatch.DecidesAt(t.TemplateAnnotations) || policymatch.DecidesAt(t.ObjectAnnotations) {
			t.PolicyName, level = policymatch.ResolvePolicy(t.TemplateAnnotations, t.ObjectAnnotations, nil)
		} else {
			a, err := nsAnn.get(ctx, t.Namespace)
			if err != nil {
				return nil, err
			}
			t.PolicyName, level = policymatch.ResolvePolicy(t.TemplateAnnotations, t.ObjectAnnotations, a)
		}
		if t.PolicyName != "" {
			logger.V(1).Info("resolved workload opt-in",
				"kind", t.Kind, "namespace", t.Namespace, "name", t.Name,
				"policy", t.PolicyName, "level", level)
		}
	}

	filtered := filterTargets(targets, policy, r.ExcludedNamespaces)
	logger.V(1).Info("filtered targets",
		"raw", len(targets),
		"matching", len(filtered),
		"excludedNamespaces", r.ExcludedNamespaces)

	// Reported here, AFTER filtering, not in listBarePodTargets: filterTargets
	// has already narrowed targets to policy.Name, so exactly one reconcile
	// reaches this branch for a given group instead of every policy whose
	// selector happens to cover the namespace. It still logs once per reconcile
	// interval while the conflict persists, deliberately — a silently-dropped
	// bare pod would be the worse bug.
	for i := range filtered {
		t := &filtered[i]
		if t.Kind != "Pod" || len(t.BarePodPolicyMismatched) == 0 {
			continue
		}
		// Re-resolved rather than read straight off p.Annotations: a mismatched
		// pod's policy may have come from its Namespace, so the raw annotation
		// is empty and the log line would print "pod-2=". No new apiserver read
		// — listBarePodTargets already warmed nsAnn via forPods.
		nsA, err := nsAnn.get(ctx, t.Namespace)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(t.BarePodPolicyMismatched))
		for _, p := range t.BarePodPolicyMismatched {
			resolvedName, _ := policymatch.ResolvePolicy(p.Annotations, nil, nsA)
			names = append(names, p.Name+"="+resolvedName)
		}
		logger.Info("bare pods share an owner-name identity but name a different policy; "+
			"they are excluded from the group and will not be rightsized under it — "+
			"give them their own k8s.sustain.io/owner-name or align the policy annotation",
			"namespace", t.Namespace, "ownerName", t.IdentityName, "groupPolicy", t.PolicyName,
			"excluded", names)
	}
	return filtered, nil
}

// dedupeNamespaces removes repeats from a namespace list, preserving the order
// of first appearance. A nil or empty list is returned unchanged: that is what
// means "all namespaces" to every caller.
//
// policy.Spec.Selector.Namespaces is a plain atomic array with no uniqueness
// constraint, so the apiserver accepts `namespaces: [prod, prod]`. Listing it
// verbatim produced the same workload twice, and the two copies were dispatched
// to two errgroup goroutines that then raced each other over per-workload state
// — the retry tracker (one goroutine's recordSuccess deleting the entry the
// other had just written) and the WorkloadRecommendation write.
func dedupeNamespaces(namespaces []string) []string {
	if len(namespaces) < 2 {
		return namespaces
	}
	seen := make(map[string]struct{}, len(namespaces))
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	return out
}

// listedKinds is the order kinds are listed in; "Pod" means bare-pod
// identities formed via the owner-name annotation.
var listedKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "Rollout", "CronJob", "Job", "Pod"}

// listInNamespaces runs list once per namespace (or once cluster-wide when
// namespaces is empty) and concatenates the results.
//
// The namespace list is deduped here rather than at the call sites: this is
// the single funnel every kind's listing goes through, so one workload can
// never be emitted as two targets no matter which selector produced the list.
func listInNamespaces(namespaces []string, list func(opts ...client.ListOption) ([]workloadTarget, error)) ([]workloadTarget, error) {
	if len(namespaces) == 0 {
		return list()
	}
	var all []workloadTarget
	for _, ns := range dedupeNamespaces(namespaces) {
		t, err := list(client.InNamespace(ns))
		if err != nil {
			return nil, err
		}
		all = append(all, t...)
	}
	return all, nil
}

// listTargetsOfKind lists every object of a template-bearing kind as targets.
//
// Jobs owned by a CronJob are skipped (the CronJob path resizes their pods,
// so listing them here would double-process), as are terminal Jobs (Complete
// or Failed — no running pods to resize). Neither the Job nor the CronJob spec
// is ever mutated; reconcile resizes the running pods in place.
func (r *PolicyReconciler) listTargetsOfKind(ctx context.Context, kind string, namespaces []string) ([]workloadTarget, error) {
	return listInNamespaces(namespaces, func(opts ...client.ListOption) ([]workloadTarget, error) {
		list := workload.ListForKind(kind)
		if list == nil {
			return nil, fmt.Errorf("unsupported kind %q", kind)
		}
		if err := r.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		var out []workloadTarget
		err := meta.EachListItem(list, func(o runtime.Object) error {
			obj := o.(client.Object)
			if job, ok := obj.(*batchv1.Job); ok &&
				(workload.IsOwnedByKind(job.OwnerReferences, "CronJob") || jobIsTerminal(job)) {
				return nil
			}
			out = append(out, targetFromObject(obj, kind))
			return nil
		})
		return out, err
	})
}

// listBarePodTargets discovers pods with no controller owner that opt into
// owner-name-based identity grouping (api/v1alpha1.OwnerNameAnnotation).
// Unlike every other kind, the policy and owner-name annotations live directly
// on the Pod — there is no pod template to read them from. Pods sharing the
// same (namespace, owner-name) collapse into one workloadTarget; cross-namespace
// grouping is out of scope, as kube_pod_labels joins are namespace-scoped too.
//
// The grouping is workload.GroupBarePods, shared with the dashboard so the
// namespace+owner-name rule and the most-recently-created-pod-wins tie-break
// have exactly one implementation.
//
// Each target carries its group's full member list (BarePodMembers) so the
// apply phase never has to re-List the namespace and regroup per identity.
//
// Pods sharing an identity but naming a different policy are dropped by the
// grouping and carried on the target (BarePodPolicyMismatched) rather than
// logged here: this function runs once per policy whose selector merely covers
// the namespace, before filterTargets narrows the result to the group's own
// policy, so logging here would fire for uninvolved policies too.
func (r *PolicyReconciler) listBarePodTargets(ctx context.Context, namespaces []string, nsAnn *nsAnnotations) ([]workloadTarget, error) {
	// GroupBarePods filters by resolved policy as it groups, so it needs the
	// namespace level up front rather than after the fact.
	return listInNamespaces(namespaces, func(opts ...client.ListOption) ([]workloadTarget, error) {
		var l corev1.PodList
		if err := r.List(ctx, &l, opts...); err != nil {
			return nil, err
		}
		nsMap, err := nsAnn.forPods(ctx, l.Items)
		if err != nil {
			return nil, err
		}
		var out []workloadTarget
		for _, g := range workload.GroupBarePods(l.Items, nsMap) {
			out = append(out, workloadTarget{
				Kind:           "Pod",
				IdentityKind:   "Pod",
				Name:           g.Name,
				IdentityName:   g.Name,
				Namespace:      g.Namespace,
				PolicyName:     g.PolicyName,
				Labels:         g.Labels,
				Containers:     g.Containers,
				InitContainers: g.InitContainers,
				Object:         g.Representative,
				// The apply phase needs every member, and this grouping is
				// the only place it is computed — see BarePodMembers.
				BarePodMembers: g.Members,
				// See BarePodPolicyMismatched: logged by collectTargets,
				// not here.
				BarePodPolicyMismatched: g.PolicyMismatched,
			})
		}
		return out, nil
	})
}
