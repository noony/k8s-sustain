package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// wlrName builds the WorkloadRecommendation object name for a workload target.
// Format: "<lowercase-kind>-<name>". Two workloads of different kinds with the
// same name (e.g. a Deployment "web" and a StatefulSet "web") get distinct
// recommendation objects within the same namespace.
func wlrName(kind, name string) string {
	return fmt.Sprintf("%s-%s", strings.ToLower(kind), name)
}

// wlrPolicyLabel labels each WorkloadRecommendation with the Policy that
// produced it. Used to scope sweep/cleanup list calls server-side instead of
// pulling every WLR in the cluster and post-filtering by spec.policy.
const wlrPolicyLabel = "k8s.sustain.io/policy"

// upsertWorkloadRecommendation writes (or updates) a WorkloadRecommendation
// for the given target. Idempotent: if the existing status matches the new
// recommendations, no API call is made. This keeps etcd write amplification
// low even at thousands of workloads × 5-min reconcile.
//
// Errors are logged but never propagated up — the cache is a fallback path,
// not load-bearing. A failed write only means the webhook may serve a slightly
// older cached value during the next Prometheus outage.
func (r *PolicyReconciler) upsertWorkloadRecommendation(
	ctx context.Context,
	t *workloadTarget,
	policyName string,
	recs map[string]workload.ContainerRecommendation,
	now metav1.Time,
) {
	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)

	desired := buildWLRStatus(recs, now)
	if len(desired.Containers) == 0 {
		// Nothing useful to cache. Skip — leave any existing object alone.
		return
	}

	key := types.NamespacedName{Namespace: t.Namespace, Name: wlrName(t.Kind, t.Name)}
	var existing sustainv1alpha1.WorkloadRecommendation
	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		obj := &sustainv1alpha1.WorkloadRecommendation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
				Labels:    map[string]string{wlrPolicyLabel: policyName},
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{
				WorkloadRef: sustainv1alpha1.WorkloadReference{
					Kind: t.Kind, Namespace: t.Namespace, Name: t.Name,
				},
				Policy: policyName,
			},
		}
		if err := r.Create(ctx, obj); err != nil {
			logger.V(1).Info("failed to create WorkloadRecommendation; skipping cache write", "err", err)
			return
		}
		// Refresh after create to pick up resourceVersion.
		if err := r.Get(ctx, key, &existing); err != nil {
			logger.V(1).Info("failed to re-read WorkloadRecommendation after create", "err", err)
			return
		}
	} else if err != nil {
		logger.V(1).Info("failed to read WorkloadRecommendation", "err", err)
		return
	}

	// Sync spec.workloadRef + policy + sweep label if drifted. Also backfills
	// the label on WLRs created before label-based sweeping was introduced.
	specChanged := existing.Spec.WorkloadRef.Kind != t.Kind ||
		existing.Spec.WorkloadRef.Namespace != t.Namespace ||
		existing.Spec.WorkloadRef.Name != t.Name ||
		existing.Spec.Policy != policyName
	labelChanged := existing.Labels[wlrPolicyLabel] != policyName
	if specChanged || labelChanged {
		patched := existing.DeepCopy()
		patched.Spec.WorkloadRef = sustainv1alpha1.WorkloadReference{
			Kind: t.Kind, Namespace: t.Namespace, Name: t.Name,
		}
		patched.Spec.Policy = policyName
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[wlrPolicyLabel] = policyName
		if err := r.Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
			logger.V(1).Info("failed to patch WorkloadRecommendation spec", "err", err)
			return
		}
		existing = *patched
	}

	if statusEquivalent(existing.Status, desired) {
		// No-op: same recommendation as last time. Skip the etcd write.
		return
	}

	patched := existing.DeepCopy()
	patched.Status = desired
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
		logger.V(1).Info("failed to patch WorkloadRecommendation status", "err", err)
	}
}

// buildWLRStatus converts the in-memory recommendation map into the CRD shape.
func buildWLRStatus(recs map[string]workload.ContainerRecommendation, now metav1.Time) sustainv1alpha1.WorkloadRecommendationStatus {
	out := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt: now,
		Source:     "prometheus",
		Containers: map[string]sustainv1alpha1.ContainerRecommendation{},
	}
	for name, rec := range recs {
		out.Containers[name] = sustainv1alpha1.ContainerRecommendation{
			CPURequest:        rec.CPURequest,
			MemoryRequest:     rec.MemoryRequest,
			CPULimit:          rec.CPULimit,
			MemoryLimit:       rec.MemoryLimit,
			RemoveCPULimit:    rec.RemoveCPULimit,
			RemoveMemoryLimit: rec.RemoveMemoryLimit,
		}
	}
	return out
}

// deleteWLRsWhere lists WorkloadRecommendation objects with the given options
// and deletes every item for which keep returns false. IsNotFound on delete is
// tolerated (the object is counted as deleted). Each delete failure is logged
// at V(1) via logger and the first such error is returned alongside the count
// of successful deletions. A list failure is returned (wrapped) with a zero
// count so callers can distinguish it from a partial delete failure.
//
// This is the shared core of the three WLR cleanup paths (per-cycle sweep,
// per-policy delete, orphan reaper); each wraps it with its own list options,
// keep predicate, error handling and aggregate logging.
func (r *PolicyReconciler) deleteWLRsWhere(
	ctx context.Context,
	logger logr.Logger,
	listOpts []client.ListOption,
	keep func(*sustainv1alpha1.WorkloadRecommendation) bool,
) (deleted int, listErr error, deleteErr error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(ctx, &list, listOpts...); err != nil {
		return 0, err, nil
	}

	for i := range list.Items {
		wlr := &list.Items[i]
		if keep(wlr) {
			continue
		}
		if err := r.Delete(ctx, wlr); err != nil && !apierrors.IsNotFound(err) {
			logger.V(1).Info("failed to delete WorkloadRecommendation",
				"name", wlr.Name, "namespace", wlr.Namespace, "policy", wlr.Spec.Policy, "err", err)
			if deleteErr == nil {
				deleteErr = err
			}
			continue
		}
		deleted++
	}
	return deleted, nil, deleteErr
}

// sweepWorkloadRecommendations deletes WorkloadRecommendation objects that
// were created by this policy but whose target workload no longer appears in
// the current target set. Called once per Reconcile after a successful pass.
//
// Best-effort: errors are logged, never returned. A missed sweep just leaves
// a stale cache entry until the next cycle.
func (r *PolicyReconciler) sweepWorkloadRecommendations(ctx context.Context, policyName string, targets []workloadTarget) {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	wanted := make(map[string]struct{}, len(targets))
	for i := range targets {
		t := &targets[i]
		wanted[t.Namespace+"/"+wlrName(t.Kind, t.Name)] = struct{}{}
	}

	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			// Defensive: if the label is stale relative to spec.policy (e.g. on
			// WLRs migrated mid-rename), keep filtering by spec.policy too.
			if wlr.Spec.Policy != policyName {
				return true
			}
			_, ok := wanted[wlr.Namespace+"/"+wlr.Name]
			return ok
		})
	if listErr != nil {
		logger.V(1).Info("failed to list WorkloadRecommendations for sweep", "err", listErr)
		return
	}
	if deleted > 0 {
		logger.V(1).Info("swept stale WorkloadRecommendations", "deleted", deleted)
	}
}

// deleteAllRecommendationsForPolicy removes every WorkloadRecommendation tied
// to the named policy. Called from the deletion branch of Reconcile before
// the cleanup finalizer is dropped — guarantees the cache doesn't outlive
// the parent Policy on the normal `kubectl delete policy` path.
//
// Returns an error so the finalizer is only removed once cleanup finishes;
// otherwise a transient API failure mid-delete would orphan WLRs.
func (r *PolicyReconciler) deleteAllRecommendationsForPolicy(ctx context.Context, policyName string) error {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	deleted, listErr, deleteErr := r.deleteWLRsWhere(ctx, logger,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			// Defensive: belt-and-braces against label drift on legacy WLRs.
			return wlr.Spec.Policy != policyName
		})
	if listErr != nil {
		return fmt.Errorf("listing WorkloadRecommendations for policy delete: %w", listErr)
	}
	if deleted > 0 {
		logger.Info("deleted WorkloadRecommendations for removed policy", "deleted", deleted)
	}
	return deleteErr
}

// reapOrphanedRecommendations is the belt-and-braces sweeper: it lists every
// WorkloadRecommendation in the cluster and deletes any whose spec.policy
// does not reference an existing Policy. Catches:
//   - WLRs left behind by `kubectl delete policy --grace-period=0 --force`
//     (which skips finalizers entirely).
//   - WLRs from a controller crash mid-delete.
//   - WLRs orphaned by a Policy renamed before the per-policy sweep ran.
//
// Best-effort and idempotent. Safe to run on a tick.
func (r *PolicyReconciler) reapOrphanedRecommendations(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("orphan-reaper")

	var policies sustainv1alpha1.PolicyList
	if err := r.List(ctx, &policies); err != nil {
		return fmt.Errorf("listing policies: %w", err)
	}
	known := make(map[string]struct{}, len(policies.Items))
	for i := range policies.Items {
		known[policies.Items[i].Name] = struct{}{}
	}

	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger, nil,
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			if wlr.Spec.Policy == "" {
				// Untracked entry — leave it; some other writer may own it.
				return true
			}
			_, ok := known[wlr.Spec.Policy]
			return ok
		})
	if listErr != nil {
		return fmt.Errorf("listing workloadrecommendations: %w", listErr)
	}
	if deleted > 0 {
		logger.Info("reaped orphan WorkloadRecommendations", "deleted", deleted)
	}
	return nil
}

// statusEquivalent reports whether two WLR statuses convey the same
// recommendation values, ignoring ObservedAt. Used to suppress no-op writes
// so write amplification scales with *change*, not workload count.
func statusEquivalent(a, b sustainv1alpha1.WorkloadRecommendationStatus) bool {
	if a.Source != b.Source {
		return false
	}
	if len(a.Containers) != len(b.Containers) {
		return false
	}
	for name, av := range a.Containers {
		bv, ok := b.Containers[name]
		if !ok {
			return false
		}
		if !quantityEqual(av.CPURequest, bv.CPURequest) ||
			!quantityEqual(av.MemoryRequest, bv.MemoryRequest) ||
			!quantityEqual(av.CPULimit, bv.CPULimit) ||
			!quantityEqual(av.MemoryLimit, bv.MemoryLimit) ||
			av.RemoveCPULimit != bv.RemoveCPULimit ||
			av.RemoveMemoryLimit != bv.RemoveMemoryLimit {
			return false
		}
	}
	return true
}
