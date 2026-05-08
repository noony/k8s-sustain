package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/workload"
)

// patchCronJobTemplate applies the recommendations to the CronJob's
// JobTemplate pod spec and Patches the CronJob in place. Future job runs
// pick up the new resources naturally; in-flight jobs finish on their
// existing spec. No pods are listed or recycled.
func (r *PolicyReconciler) patchCronJobTemplate(ctx context.Context, t *workloadTarget, recs map[string]workload.ContainerRecommendation) error {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)

	cj, ok := t.Object.(*batchv1.CronJob)
	if !ok {
		return fmt.Errorf("cronjob target carries unexpected object type %T", t.Object)
	}

	base := cj.DeepCopy()
	// changed is true whenever a recommendation field is set, regardless of
	// whether the new value differs from the existing one. This is fine — the
	// MergeFrom patch is a no-op at the API server level when the rendered
	// JSON is identical, so a redundant call costs nothing meaningful.
	changed := workload.ApplyRecommendationsToPodSpec(&cj.Spec.JobTemplate.Spec.Template.Spec, recs)
	if !changed {
		logger.V(1).Info("cronjob template recs are empty; nothing to patch")
		return nil
	}

	if err := r.Patch(ctx, cj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching cronjob jobTemplate: %w", err)
	}
	logger.Info("patched cronjob jobTemplate resources")
	return nil
}
