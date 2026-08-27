package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
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
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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

// reconcilerForPolicy wires up a PolicyReconciler with the bits SetupWithManager
// would normally inject (patcher, recorder, retries) plus a mock Prometheus.
// Returns the reconciler and the Prometheus mock server (caller closes).
func reconcilerForPolicy(t *testing.T, policy *sustainv1alpha1.Policy, extra ...runtime.Object) (*PolicyReconciler, *httptest.Server) {
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		// Always return empty samples — exercises the "no recommendations yet" branch.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	pc, err := promclient.New(server.URL)
	if err != nil {
		server.Close()
		t.Fatalf("prometheus client: %v", err)
	}

	objs := []runtime.Object{policy}
	objs = append(objs, extra...)

	// WorkloadRecommendation needs its status subresource registered: the fake
	// client rejects Status().Patch outright for types it was not told about,
	// and the WLR-driven computation phase reads the observed-resources
	// snapshot that discovery writes there.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...).
		Build()

	r := &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		QueryShardMaxSamples:     10_000_000,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, false),
		retries:                  newRetryTracker(),
	}
	return r, server
}

// promServerForReconcile creates a Prometheus mock that returns predictable per-container
// CPU/memory totals and replica counts so reconcileWorkload computes a
// recommendation deterministically.
func promServerForReconcile(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_oom_24h"):
			// No recent OOMs in tests by default.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case strings.Contains(q, "workload_max_pod_cpu"):
			// Per-pod p95 of the busiest replica = 100m.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"67108864"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
}

// reconcilerWithProm wires up a fully-populated PolicyReconciler against a
// mock Prometheus and a fake k8s cluster preloaded with extra. inPlace controls
// the patcher mode.
func reconcilerWithProm(t *testing.T, server *httptest.Server, inPlace bool, extra ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = rolloutsv1alpha1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	// See reconcilerForPolicy: the WLR status subresource must be registered or
	// the fake client refuses every discovery/computation status write.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(extra...).
		Build()

	return &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		QueryShardMaxSamples:     10_000_000,
		InPlaceUpdates:           inPlace,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, inPlace),
		retries:                  newRetryTracker(),
	}
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
	t *testing.T, server *httptest.Server, lists *atomic.Int32, objs ...runtime.Object,
) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*sustainv1alpha1.WorkloadRecommendationList); ok && lists.Add(1) == 1 {
					return nil // cache has not caught up: nothing to see yet
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	return &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		QueryShardMaxSamples:     10_000_000,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, false),
		retries:                  newRetryTracker(),
	}
}

// reconcilerCountingWLRStatusWrites is reconcilerWithProm plus a counter of
// every WorkloadRecommendation status patch the reconcile issues. Status writes
// are the unit the WLR write path is judged in: a stable workload must cost
// none, so a test that cannot count them cannot tell a converged cache from one
// being rewritten every cycle.
func reconcilerCountingWLRStatusWrites(
	t *testing.T, server *httptest.Server, writes *atomic.Int32, objs ...runtime.Object,
) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = rolloutsv1alpha1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...).
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

	return &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 4,
		QueryShardMaxSamples:     10_000_000,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, false),
		retries:                  newRetryTracker(),
	}
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
// WorkloadRecommendation to carry something in particular — its
// CreationTimestamp is what the workload-age gate reads for identities whose
// own object age says nothing.
func itemForTargetWithWLR(t *workloadTarget, wlr *sustainv1alpha1.WorkloadRecommendation) computeItem {
	ref := sustainv1alpha1.WorkloadReference{Kind: t.IdentityKind, Namespace: t.Namespace, Name: t.IdentityName}
	wlr.Spec.WorkloadRef = ref
	return computeItem{
		WLR:      wlr,
		Targets:  []*workloadTarget{t},
		Identity: promclient.WorkloadIdentity{Namespace: t.Namespace, OwnerKind: t.IdentityKind, OwnerName: t.IdentityName},
		Observed: mergedObservedResources([]*workloadTarget{t}),
	}
}

// runComputeAndApply drives BOTH phases Reconcile runs for a single-member
// identity: one computation for the identity (which also writes its
// WorkloadRecommendation), then the apply for its one member — including the
// computation's error, exactly as Reconcile threads it through, so a Prometheus
// failure still surfaces through handleStepError and the retry tracker.
func runComputeAndApply(ctx context.Context, r *PolicyReconciler, policy *sustainv1alpha1.Policy, it computeItem) error {
	snap := autoscaler.NewNamespacedSnapshot(r.Client)
	recs, err := r.computeIdentity(ctx, policy, it, snap, nil, nil, false)
	return r.reconcileWorkload(ctx, policy, it.Targets[0], snap, recs, err)
}

// promServerFor is a Prometheus stub that serves one identity's CPU and
// memory samples. The sharded batch attributes samples back to an identity via
// the namespace/owner_kind/owner_name labels, so a bare {container} metric
// would silently resolve to no inputs at all.
func promServerFor(ns, ownerKind, ownerName string) *httptest.Server {
	metric := fmt.Sprintf(`{"namespace":%q,"owner_kind":%q,"owner_name":%q,"container":"app"}`, ns, ownerKind, ownerName)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		q := req.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":%s,"value":[0,"0.1"]}]}}`, metric)
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":%s,"value":[0,"67108864"]}]}}`, metric)
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
}
