package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/workload"
)

// resizeJobPods resizes the running pods of a single standalone Job in place.
// The Job spec is never mutated and pods are never evicted — a standalone Job
// has no "next run", so the running pod is the only thing worth correcting,
// and killing it would discard in-flight work. CronJob-owned Jobs are handled
// by resizeCronJobPods; the listing path already excludes them here.
//
// Returns the number of pods whose resize the API server actually accepted, so
// the caller can suppress the ResourcesUpdated event when nothing was touched
// (terminal job, no running pods, in-place unsupported, or every resize
// rejected/skipped).
func (r *PolicyReconciler) resizeJobPods(ctx context.Context, t *workloadTarget, recs map[string]workload.ContainerRecommendation, tol workload.Tolerance, observe func(resource string)) (int, error) {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)

	job, ok := t.Object.(*batchv1.Job)
	if !ok {
		return 0, fmt.Errorf("job target carries unexpected object type %T", t.Object)
	}
	if jobIsTerminal(job) {
		logger.V(1).Info("job is terminal; nothing to resize")
		return 0, nil
	}

	jobPods, err := r.listPodsForJob(ctx, job)
	if err != nil {
		return 0, fmt.Errorf("listing pods for job %s: %w", job.Name, err)
	}
	if len(jobPods) == 0 {
		logger.V(1).Info("no running pods for job; nothing to resize")
		return 0, nil
	}

	pods := make([]*corev1.Pod, 0, len(jobPods))
	for i := range jobPods {
		pods = append(pods, &jobPods[i])
	}

	logger.V(1).Info("resizing job pods", "pods", len(pods))
	resized, err := r.patcher.ResizePodsInPlace(ctx, pods, recs,
		workload.WithTolerance(tol), workload.WithSuppressionObserver(observe))
	if err != nil {
		return 0, err
	}
	return resized, nil
}
