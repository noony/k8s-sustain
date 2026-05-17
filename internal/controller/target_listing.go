package controller

import (
	"context"
	"fmt"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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
		"cronjob", types.CronJob)

	if types.Deployment != nil && *types.Deployment == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDeploymentTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing deployments: %w", err)
		}
		logger.V(1).Info("listed deployments", "count", len(t))
		targets = append(targets, t...)
	}

	if types.StatefulSet != nil && *types.StatefulSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listStatefulSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing statefulsets: %w", err)
		}
		logger.V(1).Info("listed statefulsets", "count", len(t))
		targets = append(targets, t...)
	}

	if types.DaemonSet != nil && *types.DaemonSet == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listDaemonSetTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing daemonsets: %w", err)
		}
		logger.V(1).Info("listed daemonsets", "count", len(t))
		targets = append(targets, t...)
	}

	if types.ArgoRollout != nil && *types.ArgoRollout == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listRolloutTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing rollouts: %w", err)
		}
		logger.V(1).Info("listed argo rollouts", "count", len(t))
		targets = append(targets, t...)
	}

	if types.CronJob != nil && *types.CronJob == sustainv1alpha1.UpdateModeOngoing {
		t, err := r.listCronJobTargets(ctx, namespaces)
		if err != nil {
			return nil, fmt.Errorf("listing cronjobs: %w", err)
		}
		logger.V(1).Info("listed cronjobs", "count", len(t))
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
// CronJob targets carry the JobTemplate's pod spec; reconcile patches the
// JobTemplate in place rather than recycling pods (jobs run to completion).
func (r *PolicyReconciler) listCronJobTargets(ctx context.Context, namespaces []string) ([]workloadTarget, error) {
	return listKindTargets(ctx, r.Client, namespaces,
		func() *batchv1.CronJobList { return &batchv1.CronJobList{} },
		func(l *batchv1.CronJobList, out *[]workloadTarget) {
			for i := range l.Items {
				*out = append(*out, cronJobToTarget(&l.Items[i]))
			}
		})
}
