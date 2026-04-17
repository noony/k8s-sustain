package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s.sustain.io,resources=policies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

// PolicyReconciler reconciles a Policy object.
type PolicyReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	PrometheusClient   *promclient.Client
	ReconcileInterval  time.Duration
	InPlaceUpdates     bool
	ExcludedNamespaces []string
	RecommendOnly      bool
	recorder           record.EventRecorder
	patcher            *workload.Patcher
}

func (r *PolicyReconciler) isExcluded(namespace string) bool {
	return slices.Contains(r.ExcludedNamespaces, namespace)
}

// SetupWithManager registers the PolicyReconciler with the given manager.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.patcher = workload.New(r.Client, r.InPlaceUpdates)
	r.recorder = mgr.GetEventRecorderFor("k8s-sustain")
	return ctrl.NewControllerManagedBy(mgr).
		For(&sustainv1alpha1.Policy{}).
		Complete(r)
}

// Reconcile is the main reconciliation loop for Policy objects.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.PrometheusClient == nil {
		return ctrl.Result{}, fmt.Errorf("prometheus client not configured")
	}

	policy := &sustainv1alpha1.Policy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: remove finalizer and let garbage collection clean up.
	const finalizerName = "k8s.sustain.io/cleanup"
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(policy, finalizerName) {
			r.recorder.Event(policy, corev1.EventTypeNormal, "Cleanup", "Policy deleted, removing finalizer.")
			controllerutil.RemoveFinalizer(policy, finalizerName)
			if err := r.Update(ctx, policy); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(policy, finalizerName) {
		controllerutil.AddFinalizer(policy, finalizerName)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	timer := prometheus.NewTimer(reconcileDuration.WithLabelValues(policy.Name))
	defer timer.ObserveDuration()

	var errs []error
	if err := r.reconcileDeployments(ctx, policy); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileStatefulSets(ctx, policy); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileDaemonSets(ctx, policy); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileCronJobs(ctx, policy); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		combined := errors.Join(errs...)
		_ = r.failCondition(ctx, policy, "ReconciliationFailed", combined)
		r.recorder.Event(policy, corev1.EventTypeWarning, "ReconciliationFailed", combined.Error())
		reconcileTotal.WithLabelValues(policy.Name, "error").Inc()
	} else {
		_ = r.setCondition(ctx, policy, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ReconciliationSucceeded",
			Message:            "All targeted workloads have been processed.",
			ObservedGeneration: policy.Generation,
		})
		r.recorder.Event(policy, corev1.EventTypeNormal, "ReconciliationSucceeded", "All targeted workloads have been processed.")
		reconcileTotal.WithLabelValues(policy.Name, "success").Inc()
	}

	return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
}

// reconcileDeployments lists all Deployments cluster-wide whose pod template
// carries the policy annotation pointing to this policy, then applies Ongoing
// recommendations. OnCreate is skipped — the admission webhook owns that path.
func (r *PolicyReconciler) reconcileDeployments(ctx context.Context, policy *sustainv1alpha1.Policy) error {
	if policy.Spec.Update.Types.Deployment == nil ||
		*policy.Spec.Update.Types.Deployment == sustainv1alpha1.UpdateModeOnCreate {
		return nil
	}
	mode := *policy.Spec.Update.Types.Deployment
	logger := log.FromContext(ctx).WithValues("kind", "Deployment")

	var list appsv1.DeploymentList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}

	for i := range list.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := &list.Items[i]
		if r.isExcluded(d.Namespace) {
			continue
		}
		if d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policy.Name {
			continue
		}
		wl := logger.WithValues("deployment", d.Name, "namespace", d.Namespace)
		recs, err := r.buildRecommendations(ctx, policy, d.Namespace, "Deployment", d.Name, d.Spec.Template.Spec.Containers)
		if err != nil {
			wl.Error(err, "prometheus query failed")
			continue
		}
		if len(recs) == 0 {
			wl.Info("no metrics yet, skipping")
			continue
		}
		if r.RecommendOnly {
			wl.Info("recommend-only: computed recommendations", "recommendations", recs)
			continue
		}
		if err := r.patcher.PatchDeployment(ctx, d, mode, recs); err != nil {
			wl.Error(err, "patch failed")
		}
	}
	return nil
}

// reconcileStatefulSets lists all StatefulSets cluster-wide annotated with this policy.
func (r *PolicyReconciler) reconcileStatefulSets(ctx context.Context, policy *sustainv1alpha1.Policy) error {
	if policy.Spec.Update.Types.StatefulSet == nil ||
		*policy.Spec.Update.Types.StatefulSet == sustainv1alpha1.UpdateModeOnCreate {
		return nil
	}
	mode := *policy.Spec.Update.Types.StatefulSet
	logger := log.FromContext(ctx).WithValues("kind", "StatefulSet")

	var list appsv1.StatefulSetList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing statefulsets: %w", err)
	}

	for i := range list.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s := &list.Items[i]
		if r.isExcluded(s.Namespace) {
			continue
		}
		if s.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policy.Name {
			continue
		}
		wl := logger.WithValues("statefulset", s.Name, "namespace", s.Namespace)
		recs, err := r.buildRecommendations(ctx, policy, s.Namespace, "StatefulSet", s.Name, s.Spec.Template.Spec.Containers)
		if err != nil {
			wl.Error(err, "prometheus query failed")
			continue
		}
		if len(recs) == 0 {
			wl.Info("no metrics yet, skipping")
			continue
		}
		if r.RecommendOnly {
			wl.Info("recommend-only: computed recommendations", "recommendations", recs)
			continue
		}
		if err := r.patcher.PatchStatefulSet(ctx, s, mode, recs); err != nil {
			wl.Error(err, "patch failed")
		}
	}
	return nil
}

// reconcileDaemonSets lists all DaemonSets cluster-wide annotated with this policy.
func (r *PolicyReconciler) reconcileDaemonSets(ctx context.Context, policy *sustainv1alpha1.Policy) error {
	if policy.Spec.Update.Types.DaemonSet == nil ||
		*policy.Spec.Update.Types.DaemonSet == sustainv1alpha1.UpdateModeOnCreate {
		return nil
	}
	mode := *policy.Spec.Update.Types.DaemonSet
	logger := log.FromContext(ctx).WithValues("kind", "DaemonSet")

	var list appsv1.DaemonSetList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing daemonsets: %w", err)
	}

	for i := range list.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ds := &list.Items[i]
		if r.isExcluded(ds.Namespace) {
			continue
		}
		if ds.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policy.Name {
			continue
		}
		wl := logger.WithValues("daemonset", ds.Name, "namespace", ds.Namespace)
		recs, err := r.buildRecommendations(ctx, policy, ds.Namespace, "DaemonSet", ds.Name, ds.Spec.Template.Spec.Containers)
		if err != nil {
			wl.Error(err, "prometheus query failed")
			continue
		}
		if len(recs) == 0 {
			wl.Info("no metrics yet, skipping")
			continue
		}
		if r.RecommendOnly {
			wl.Info("recommend-only: computed recommendations", "recommendations", recs)
			continue
		}
		if err := r.patcher.PatchDaemonSet(ctx, ds, mode, recs); err != nil {
			wl.Error(err, "patch failed")
		}
	}
	return nil
}

// reconcileCronJobs lists all CronJobs cluster-wide annotated with this policy
// and updates their job template resources for future runs.
func (r *PolicyReconciler) reconcileCronJobs(ctx context.Context, policy *sustainv1alpha1.Policy) error {
	if policy.Spec.Update.Types.CronJob == nil ||
		*policy.Spec.Update.Types.CronJob == sustainv1alpha1.UpdateModeOnCreate {
		return nil // OnCreate is handled by the admission webhook for each job pod
	}
	logger := log.FromContext(ctx).WithValues("kind", "CronJob")

	var list batchv1.CronJobList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing cronjobs: %w", err)
	}

	for i := range list.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cj := &list.Items[i]
		if r.isExcluded(cj.Namespace) {
			continue
		}
		if cj.Spec.JobTemplate.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] != policy.Name {
			continue
		}
		wl := logger.WithValues("cronjob", cj.Name, "namespace", cj.Namespace)
		recs, err := r.buildRecommendations(ctx, policy, cj.Namespace, "CronJob", cj.Name,
			cj.Spec.JobTemplate.Spec.Template.Spec.Containers)
		if err != nil {
			wl.Error(err, "prometheus query failed")
			continue
		}
		if len(recs) == 0 {
			wl.Info("no metrics yet, skipping")
			continue
		}
		if r.RecommendOnly {
			wl.Info("recommend-only: computed recommendations", "recommendations", recs)
			continue
		}
		if err := r.patcher.PatchCronJob(ctx, cj, recs); err != nil {
			wl.Error(err, "patch failed")
		}
	}
	return nil
}

// buildRecommendations queries Prometheus and computes per-container recommendations
// for the given workload. Returns an empty map when no data is available yet.
func (r *PolicyReconciler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	cpuQuantile := recommender.PercentileQuantile(rsCfg.CPU.Requests.PercentilePercentage)
	cpuWindow := recommender.ResourceWindow(rsCfg.CPU.Window)
	memQuantile := recommender.PercentileQuantile(rsCfg.Memory.Requests.PercentilePercentage)
	memWindow := recommender.ResourceWindow(rsCfg.Memory.Window)

	cpuValues, err := r.PrometheusClient.QueryCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}

	memValues, err := r.PrometheusClient.QueryMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		var rec workload.ContainerRecommendation
		hasData := false

		if cores, ok := cpuValues[c.Name]; ok {
			rec.CPURequest = recommender.ComputeCPURequest(cores, rsCfg.CPU.Requests)
			lr := recommender.ComputeLimit(rec.CPURequest, c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu(), rsCfg.CPU.Limits)
			rec.CPULimit = lr.Quantity
			rec.RemoveCPULimit = lr.Remove
			hasData = true
		}

		if bytes, ok := memValues[c.Name]; ok {
			rec.MemoryRequest = recommender.ComputeMemoryRequest(bytes, rsCfg.Memory.Requests)
			lr := recommender.ComputeLimit(rec.MemoryRequest, c.Resources.Requests.Memory(), c.Resources.Limits.Memory(), rsCfg.Memory.Limits)
			rec.MemoryLimit = lr.Quantity
			rec.RemoveMemoryLimit = lr.Remove
			hasData = true
		}

		if hasData {
			recs[c.Name] = rec
		}
	}

	return recs, nil
}

// setCondition upserts a status condition on policy, preserving LastTransitionTime
// when the status is unchanged, then persists via the status subresource.
func (r *PolicyReconciler) setCondition(ctx context.Context, policy *sustainv1alpha1.Policy, cond metav1.Condition) error {
	cond.LastTransitionTime = metav1.Now()

	for i, c := range policy.Status.Conditions {
		if c.Type != cond.Type {
			continue
		}
		if c.Status == cond.Status {
			cond.LastTransitionTime = c.LastTransitionTime // no change in status, keep time
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
