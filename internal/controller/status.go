package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// setCondition upserts a status condition on policy, preserving LastTransitionTime
// when the status is unchanged, then persists via the status subresource.
func (r *PolicyReconciler) setCondition(ctx context.Context, policy *sustainv1alpha1.Policy, cond metav1.Condition) error {
	cond.LastTransitionTime = metav1.Now()

	for i, c := range policy.Status.Conditions {
		if c.Type != cond.Type {
			continue
		}
		if c.Status == cond.Status {
			cond.LastTransitionTime = c.LastTransitionTime
		}
		policy.Status.Conditions[i] = cond
		return r.Status().Update(ctx, policy)
	}

	policy.Status.Conditions = append(policy.Status.Conditions, cond)
	return r.Status().Update(ctx, policy)
}

// failCondition sets a Ready=False condition and returns the original error so the
// caller can propagate it to the controller-runtime retry machinery.
func (r *PolicyReconciler) failCondition(ctx context.Context, policy *sustainv1alpha1.Policy, reason string, err error) error {
	_ = r.setCondition(ctx, policy, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            err.Error(),
		ObservedGeneration: policy.Generation,
	})
	return err
}
