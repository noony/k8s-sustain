package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func testutilCounterValue(t *testing.T, vec *prom.CounterVec, ns, kind, name, container string) float64 {
	t.Helper()
	return testutil.ToFloat64(vec.With(prom.Labels{
		"namespace": ns, "owner_kind": kind, "owner_name": name, "container": container,
	}))
}

// makeReconciler builds a PolicyReconciler with a fake client preloaded with objs.
func makeReconciler(t *testing.T, objs ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme batch: %v", err)
	}
	if err := rolloutsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme rollouts: %v", err)
	}
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	objsTyped := make([]runtime.Object, 0, len(objs))
	objsTyped = append(objsTyped, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objsTyped {
		if co, ok := o.(metav1.Object); ok {
			_ = co // keep typed
		}
	}
	c = c.WithRuntimeObjects(objsTyped...)
	return &PolicyReconciler{Client: c.Build(), Scheme: scheme}
}

func annotatedDeployment(ns, name, policy string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": name},
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}
}

func annotatedCronJob(ns, name, policy string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy},
						},
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
					},
				},
			},
		},
	}
}

// annotatedJob builds a standalone (not CronJob-owned), non-terminal Job
// opted into policy via its pod template annotation.
func annotatedJob(ns, name, policy string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-uid")},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}
}

func annotatedRollout(ns, name, policy string) *rolloutsv1alpha1.Rollout {
	return &rolloutsv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: rolloutsv1alpha1.RolloutSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": name},
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}
}

// testFullScheme registers every kind the reconciler lists or writes.
func testFullScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		appsv1.AddToScheme, autoscalingv2.AddToScheme, batchv1.AddToScheme, rolloutsv1alpha1.AddToScheme,
		sustainv1alpha1.AddToScheme, corev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return scheme
}

// reconcilerOn wires a PolicyReconciler around c with the bits
// SetupWithManager would normally inject (patcher, recorder, retries) and the
// given inputs fetcher.
func reconcilerOn(c client.Client, scheme *runtime.Scheme, inputs recommender.InputsFetcher, inPlace bool) *PolicyReconciler {
	return &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		Inputs:                   inputs,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		InPlaceUpdates:           inPlace,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, inPlace),
		retries:                  newRetryTracker(),
	}
}

// fakeClientBuilder preloads objs. WorkloadRecommendation needs its status
// subresource registered: the fake client rejects Status().Patch outright for
// types it was not told about, and the computation phase reads the
// observed-resources snapshot that discovery writes there.
func fakeClientBuilder(scheme *runtime.Scheme, objs ...runtime.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...)
}

// reconcilerForPolicy is a reconciler for policy against a Prometheus that
// knows no identity, which exercises the "no recommendations yet" branch.
func reconcilerForPolicy(t *testing.T, policy *sustainv1alpha1.Policy, extra ...runtime.Object) *PolicyReconciler {
	t.Helper()
	return reconcilerWithInputs(t, recommendertest.NewStaticInputs(), false, append([]runtime.Object{policy}, extra...)...)
}

// reconcilerWithInputs wires a fully-populated PolicyReconciler against inputs
// and a fake cluster preloaded with objs. inPlace controls the patcher mode.
func reconcilerWithInputs(t *testing.T, inputs recommender.InputsFetcher, inPlace bool, objs ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := testFullScheme(t)
	return reconcilerOn(fakeClientBuilder(scheme, objs...).Build(), scheme, inputs, inPlace)
}

func identityOf(ns, kind, name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: ns, OwnerKind: kind, OwnerName: name}
}

// appUsage is a per-pod p95 of 100m CPU and 64Mi memory for container "app".
func appUsage() *recommender.WorkloadInputs {
	return &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1},
		MemPerPod: promclient.ContainerValues{"app": 64 << 20},
	}
}

// usageFor serves appUsage for one identity, so the reconciler computes a
// recommendation for it deterministically.
func usageFor(ns, kind, name string) *recommendertest.StaticInputs {
	return recommendertest.NewStaticInputs().Set(identityOf(ns, kind, name), appUsage())
}

func policyForReconcileWorkload(t *testing.T, name string) *sustainv1alpha1.Policy {
	t.Helper()
	p95 := int32(95)
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
			},
		},
	}
}

func deploymentTarget(ns, name string) *workloadTarget {
	return &workloadTarget{
		Kind:         "Deployment",
		Name:         name,
		Namespace:    ns,
		IdentityKind: "Deployment",
		IdentityName: name,
		Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		}},
		Object: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}},
	}
}

// reconcilerWithLaggingWLRList builds a reconciler whose FIRST
// WorkloadRecommendationList comes back empty, whatever the store holds — the
// informer-cache lag a fake client cannot otherwise express. Every later List,
// and every other kind, behaves normally. lists counts the intercepted calls so
// a test can prove the interceptor actually fired.
func reconcilerWithLaggingWLRList(
	t *testing.T, inputs recommender.InputsFetcher, lists *atomic.Int32, objs ...runtime.Object,
) *PolicyReconciler {
	t.Helper()
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*sustainv1alpha1.WorkloadRecommendationList); ok && lists.Add(1) == 1 {
					return nil // cache has not caught up: nothing to see yet
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	return reconcilerOn(c, scheme, inputs, false)
}

// reconcilerCountingWLRStatusWrites is reconcilerWithInputs plus a counter of
// every WorkloadRecommendation status patch the reconcile issues. Status writes
// are the unit the WLR write path is judged in: a stable workload must cost
// none, so a test that cannot count them cannot tell a converged cache from one
// being rewritten every cycle.
func reconcilerCountingWLRStatusWrites(
	t *testing.T, inputs recommender.InputsFetcher, writes *atomic.Int32, objs ...runtime.Object,
) *PolicyReconciler {
	t.Helper()
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					writes.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, inputs, false)
	r.WorkloadConcurrencyLimit = 4
	return r
}

// targetFromObject builds the apply target of the single-member identity obj
// forms, as the inventory and targetsOf would.
func targetFromObject(obj client.Object, kind string) workloadTarget {
	m := inventory.Member{Object: obj}
	name := obj.GetName()
	if tmpl, _, ok := workload.PodTemplateOf(obj); ok {
		m.Containers, m.InitContainers = tmpl.Spec.Containers, tmpl.Spec.InitContainers
		_, name = workload.ApplyOwnerNameOverride(kind, name, tmpl.Annotations)
	}
	return *targetFromMember(identityOf(obj.GetNamespace(), kind, name), m, "")
}

// itemForTarget builds the computeItem the reconciler's computation phase would
// hand a single-member identity, so a test can drive computeIdentity or the WLR
// write path without standing up a full Reconcile.
func itemForTarget(t *workloadTarget) computeItem {
	return itemForTargetWithWLR(t, &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: t.Namespace,
			Name:      wlrcache.Name(t.IdentityKind, t.IdentityName),
		},
	})
}

// itemForTargetWithWLR is itemForTarget for tests that need the identity's
// WorkloadRecommendation to carry something in particular. The identity is
// dated as the inventory dates it: by its member or its WorkloadRecommendation,
// whichever is older.
func itemForTargetWithWLR(t *workloadTarget, wlr *sustainv1alpha1.WorkloadRecommendation) computeItem {
	ref := sustainv1alpha1.WorkloadReference{Kind: t.IdentityKind, Namespace: t.Namespace, Name: t.IdentityName}
	wlr.Spec.WorkloadRef = ref
	since := wlr.CreationTimestamp.Time
	if t.Object != nil {
		if created := t.Object.GetCreationTimestamp().Time; !created.IsZero() && (since.IsZero() || created.Before(since)) {
			since = created
		}
	}
	return computeItem{
		WLR:      wlr,
		Targets:  []*workloadTarget{t},
		Identity: promclient.WorkloadIdentity{Namespace: t.Namespace, OwnerKind: t.IdentityKind, OwnerName: t.IdentityName},
		Observed: wlrcache.BuildObservedResources(t.Containers, t.InitContainers),
		Since:    since,
	}
}

// runComputeAndApply drives the phases Reconcile runs for a single-member
// identity: discovery, the recommendation pass, the WorkloadRecommendation
// record, then — when the record landed — the apply for its member unless it
// is in backoff, threading a fetch failure through handleStepError and the
// retry tracker exactly as Reconcile does, and the identity's health emission.
func runComputeAndApply(ctx context.Context, r *PolicyReconciler, policy *sustainv1alpha1.Policy, it computeItem) error {
	items := []computeItem{it}
	r.discover(ctx, policy.Name, items)
	r.health.observe(policy.Name, items)
	snap := autoscaler.NewNamespacedSnapshot(r.Client)
	results := r.recommend(ctx, policy, items, snap)
	r.persist(ctx, results)
	res := results[0]
	if res.recordErr != nil {
		return res.recordErr
	}
	var err error
	for _, t := range res.apply {
		err = errors.Join(err, r.reconcileWorkload(ctx, policy, t, snap, res.recs, res.err))
	}
	r.health.emit(policy.Name, r.retries)
	return err
}
