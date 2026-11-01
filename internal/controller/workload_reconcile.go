package controller

import (
	"context"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/ledger"
	"github.com/noony/k8s-sustain/internal/workload"
)

// applyResult is what one pass did with one live member, the apply step's
// only output: the result the ledger settles, and the decreases the downsize
// threshold held back on its pods.
type applyResult struct {
	ledger.Result
	member *workloadTarget
	// suppressed counts, per resource, the member's pods on which the
	// downsize threshold held back a decrease.
	suppressed map[string]int
}

// applied is res, what the pass did with member t, keyed for the ledger.
func applied(t *workloadTarget, res ledger.Result) applyResult {
	res.Member = t.key()
	return applyResult{Result: res, member: t}
}

// apply applies every identity's verdict to its live members not in backoff,
// one task per member so a large owner-name group spreads across the slots,
// and returns, per identity, exactly one result per live member, backed-off
// ones included. It keeps no retry or health state: the ledger settles the
// results from the pass's report.
//
// An identity whose decision could not be stored is not applied: the
// replacements of the pods it would evict read the stored Recommendation at
// admission, and would start on template resources. Its members fail on the
// store step instead.
func (r *PolicyReconciler) apply(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	work []passIdentity,
	recs []recommendation,
	stored []error,
) [][]applyResult {
	logger := log.FromContext(ctx)
	out := make([][]applyResult, len(work))
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i, w := range work {
		out[i] = make([]applyResult, len(w.backedOff)+len(w.run))
		for j, t := range w.backedOff {
			logger.V(1).Info("skipping workload in retry backoff", "target", t.key().String())
			out[i][j] = applied(t, ledger.Result{Kind: ledger.Skipped})
		}
		run := out[i][len(w.backedOff):]
		if stored[i] != nil && len(w.run) > 0 {
			id := w.id.Key
			logger.Info("not applying: the identity's recommendation could not be stored for the webhook",
				"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace,
				"members", len(w.run), "err", stored[i].Error())
			for j, t := range w.run {
				run[j] = applied(t, ledger.Result{Kind: ledger.Failed, Step: workload.StepStore, Err: stored[i]})
			}
			continue
		}
		for j, t := range w.run {
			g.Go(func() error {
				run[j] = r.reconcileWorkload(ctx, policy, t, recs[i])
				return nil
			})
		}
	}
	_ = g.Wait()
	return out
}

// reconcileWorkload applies an identity's recommendation to a single workload
// target and emits its events. It does not compute anything and does not
// write the WorkloadRecommendation — the recommendation pass and its record
// did both, once for the identity, before any member reached this function.
//
// rec is the identity's verdict: its Recommendation covers the union of the
// identity's members' containers, which workload.Apply narrows to the
// member's. Its fetch failure is reported as each member's failed prometheus
// step, because retry state is kept per real workload object.
func (r *PolicyReconciler) reconcileWorkload(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	t *workloadTarget,
	rec recommendation,
) applyResult {
	// Early bail-out if the parent reconcile context has already been cancelled
	// (typically a manager shutdown mid-batch). Without this, queued workers
	// still start pod reads and writes that will each fail through their own
	// timeout path, slowing graceful shutdown.
	if ctx.Err() != nil {
		return applied(t, ledger.Result{Kind: ledger.Skipped})
	}

	logger := log.FromContext(ctx).WithValues("kind", t.Kind, "name", t.Name, "namespace", t.Namespace)
	ctx = log.IntoContext(ctx, logger)

	if info, ok := rec.autoscalers.Members[t.Name]; ok && info.Kind != autoscaler.KindNone {
		r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "AutoscalerDetected", "AutoscalerDetected",
			"%s %s detected targeting %s/%s (replicas %d–%d)",
			info.Kind, info.Name, t.Kind, t.Name, info.MinReplicas, info.MaxReplicas)
	}

	if rec.err != nil {
		return applied(t, ledger.Result{Kind: ledger.Failed, Step: workload.StepPrometheus, Err: rec.err})
	}

	res := r.patcher.Apply(ctx, t.Member, rec.recs, workload.ApplySettingsFor(policy, t.Kind, r.RecommendOnly))
	if res.Kind == workload.ResultDone && res.Changed() > 0 {
		r.recorder.Eventf(t.Object, nil, corev1.EventTypeNormal, "ResourcesUpdated", "ResourcesUpdated",
			"Updated resources on %d pod(s) (%d resized in place, %d evicted) for containers: %v",
			res.Changed(), len(res.Resized), len(res.Evicted), res.Containers)
		logger.Info("workload resources updated", "containers", res.Containers, "resized", res.Resized, "evicted", res.Evicted)
	}
	return resultOf(t, res)
}

// resultOf is the ledger's view of what Apply did with member t.
func resultOf(t *workloadTarget, res workload.Result) applyResult {
	settled := ledger.Result{Kind: ledger.Done, Pods: res.Pods}
	switch {
	case res.Kind == workload.ResultFailed:
		settled = ledger.Result{Kind: ledger.Failed, Step: res.Step, Err: res.Err}
	case res.Kind == workload.ResultIdle:
		settled = ledger.Result{Kind: ledger.Idle}
	case !res.Counted:
		settled = ledger.Result{Kind: ledger.Uncounted}
	}
	out := applied(t, settled)
	out.suppressed = res.Suppressed
	return out
}
