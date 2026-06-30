package controller

import (
	"context"
	"fmt"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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

	kinds := []struct {
		mode *sustainv1alpha1.UpdateMode
		name string
		list func(context.Context, []string) ([]workloadTarget, error)
	}{
		{types.Deployment, "deployments", r.listDeploymentTargets},
		{types.StatefulSet, "statefulsets", r.listStatefulSetTargets},
		{types.DaemonSet, "daemonsets", r.listDaemonSetTargets},
		{types.ArgoRollout, "rollouts", r.listRolloutTargets},
		{types.CronJob, "cronjobs", r.listCronJobTargets},
		{types.Job, "jobs", r.listJobTargets},
		{types.Pod, "pods", r.listBarePodTargets},
	}
	for _, k := range kinds {
		if k.mode == nil || *k.mode != sustainv1alpha1.UpdateModeOngoing {
			continue
		}
		t, err := k.list(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", k.name, err)
		}
		logger.V(1).Info("listed workloads", "kind", k.name, "count", len(t))
		targets = append(targets, t...)
	}

	filtered := filterTargets(targets, policy, r.ExcludedNamespaces)
	logger.V(1).Info("filtered targets",
		"raw", len(targets),
		"matching", len(filtered),
		"excludedNamespaces", r.ExcludedNamespaces)
	return filtered, nil
}

// listKindTargets lists objects of kind L across the given namespaces (or
// cluster-wide if namespaces is empty), converting items to workloadTargets via
// appendItems. newList must return a fresh list per call — sharing one value
// across calls would let the second List overwrite the first.
func listKindTargets[L client.ObjectList](
	ctx context.Context,
	c client.Client,
	namespaces []string,
	newList func() L,
	appendItems func(L, *[]workloadTarget),
) ([]workloadTarget, error) {
	fetch := func(opts ...client.ListOption) ([]workloadTarget, error) {
		list := newList()
		if err := c.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		var out []workloadTarget
		appendItems(list, &out)
		return out, nil
	}
	if len(namespaces) > 0 {
		var all []workloadTarget
		for _, ns := range namespaces {
			t, err := fetch(client.InNamespace(ns))
			if err != nil {
				return nil, err
			}
			all = append(all, t...)
		}
		return all, nil
	}
	return fetch()
}

func (r *PolicyReconciler) listDeploymentTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *appsv1.DeploymentList { return &appsv1.DeploymentList{} },
		func(l *appsv1.DeploymentList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, deploymentToTarget(&l.Items[i]))
			}
		})
}

func (r *PolicyReconciler) listStatefulSetTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *appsv1.StatefulSetList { return &appsv1.StatefulSetList{} },
		func(l *appsv1.StatefulSetList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, statefulSetToTarget(&l.Items[i]))
			}
		})
}

func (r *PolicyReconciler) listDaemonSetTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *appsv1.DaemonSetList { return &appsv1.DaemonSetList{} },
		func(l *appsv1.DaemonSetList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, daemonSetToTarget(&l.Items[i]))
			}
		})
}

func (r *PolicyReconciler) listRolloutTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *rolloutsv1alpha1.RolloutList { return &rolloutsv1alpha1.RolloutList{} },
		func(l *rolloutsv1alpha1.RolloutList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, rolloutToTarget(&l.Items[i]))
			}
		})
}

// listCronJobTargets lists CronJobs, scoped to namespaces if provided.
// CronJob targets carry the JobTemplate's pod spec; reconcile resizes the
// currently-running job pods in place rather than recycling them or mutating
// the CronJob spec (jobs run to completion).
func (r *PolicyReconciler) listCronJobTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *batchv1.CronJobList { return &batchv1.CronJobList{} },
		func(l *batchv1.CronJobList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, cronJobToTarget(&l.Items[i]))
			}
		})
}

// listJobTargets lists standalone Jobs, scoped to namespaces if provided.
// Jobs owned by a CronJob are skipped (the CronJob path resizes their pods,
// so listing them here would double-process), as are terminal Jobs (Complete
// or Failed — no running pods to resize). Like CronJobs, the Job spec is never
// mutated; reconcile resizes the running pods in place.
func (r *PolicyReconciler) listJobTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *batchv1.JobList { return &batchv1.JobList{} },
		func(l *batchv1.JobList, out *[]workloadTarget) {
			for i := range l.Items {
				j := &l.Items[i]
				if workload.IsOwnedByKind(j.OwnerReferences, "CronJob") {
					continue
				}
				if jobIsTerminal(j) {
					continue
				}
				*out = append(*out, jobToTarget(j))
			}
		})
}

// listBarePodTargets discovers pods with no controller owner that opt into
// owner-name-based identity grouping (api/v1alpha1.OwnerNameAnnotation).
// Unlike every other kind, the policy and owner-name annotations live
// directly on the Pod — there is no pod template to read them from. Pods
// sharing the same (namespace, owner-name) collapse into one workloadTarget,
// per the spec's namespace-scoping rule (cross-namespace grouping is out of
// scope — kube_pod_labels joins are namespace-scoped too).
//
// The grouping itself is workload.GroupBarePods, shared with the dashboard's
// listWorkloadsOfKind/getWorkloadEntry so the namespace+owner-name rule and
// the most-recently-created-pod-wins tie-break have exactly one
// implementation.
func (r *PolicyReconciler) listBarePodTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *corev1.PodList { return &corev1.PodList{} },
		func(l *corev1.PodList, out *[]workloadTarget) {
			for _, g := range workload.GroupBarePods(l.Items) {
				*out = append(*out, workloadTarget{
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
				})
			}
		})
}
