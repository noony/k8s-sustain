package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/workload"
)

// jobPodNameLabel is the label kubelet/Job controller stamps on every pod
// owned by a Job. It is the canonical way to enumerate a Job's pods without
// walking ownerReferences pod-by-pod.
const jobPodNameLabel = "batch.kubernetes.io/job-name"

// resizeCronJobPods enumerates the active Jobs owned by the CronJob target
// and resizes their pods in place. The CronJob spec is never mutated, and
// pods are never evicted — short-lived job runs must finish on their own.
// New scheduled runs pick up the latest resources from the webhook at
// admission time.
//
// Returns the number of pods actually resized so the caller can suppress
// the ResourcesUpdated event when nothing was touched (no active pods, or
// in-place resize unsupported on this cluster).
func (r *PolicyReconciler) resizeCronJobPods(ctx context.Context, t *workloadTarget, recs map[string]workload.ContainerRecommendation) (int, error) {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)

	cj, ok := t.Object.(*batchv1.CronJob)
	if !ok {
		return 0, fmt.Errorf("cronjob target carries unexpected object type %T", t.Object)
	}

	jobs, err := r.listActiveJobsForCronJob(ctx, cj)
	if err != nil {
		return 0, fmt.Errorf("listing jobs for cronjob: %w", err)
	}
	if len(jobs) == 0 {
		logger.V(1).Info("no active jobs for cronjob; nothing to resize (next run will pick up new resources via webhook)")
		return 0, nil
	}

	var pods []*corev1.Pod
	for i := range jobs {
		jobPods, err := r.listPodsForJob(ctx, &jobs[i])
		if err != nil {
			return 0, fmt.Errorf("listing pods for job %s: %w", jobs[i].Name, err)
		}
		for j := range jobPods {
			pods = append(pods, &jobPods[j])
		}
	}

	logger.V(1).Info("resizing cronjob pods", "jobs", len(jobs), "pods", len(pods))
	if err := r.patcher.ResizePodsInPlace(ctx, pods, recs); err != nil {
		return 0, err
	}
	// Checked after the call: the patcher flips InPlace to false at runtime
	// if the API server rejects the resize (feature gate disabled), in which
	// case nothing was resized.
	if !r.patcher.InPlace() {
		return 0, nil
	}
	resized := 0
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			resized++
		}
	}
	return resized, nil
}

// listActiveJobsForCronJob returns the Jobs in the CronJob's namespace that
// are owned by it (controller ownerRef) and not Complete/Failed.
func (r *PolicyReconciler) listActiveJobsForCronJob(ctx context.Context, cj *batchv1.CronJob) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := r.List(ctx, &list, client.InNamespace(cj.Namespace)); err != nil {
		return nil, err
	}
	var out []batchv1.Job
	for i := range list.Items {
		j := &list.Items[i]
		if !workload.IsOwnedBy(j.OwnerReferences, cj.UID) {
			continue
		}
		if jobIsTerminal(j) {
			continue
		}
		out = append(out, *j)
	}
	return out, nil
}

// listPodsForJob returns the pods owned by the given Job, scoped via the
// canonical batch.kubernetes.io/job-name label.
func (r *PolicyReconciler) listPodsForJob(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{jobPodNameLabel: job.Name},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// jobIsTerminal reports whether the Job has reached a terminal state
// (Complete or Failed) and therefore has no pods worth resizing.
func jobIsTerminal(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}
