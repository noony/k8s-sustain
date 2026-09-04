package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func TestDiscoverCreatesWLRForEveryTarget(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	targets := []workloadTarget{
		{
			Kind: "Deployment", Name: "api", Namespace: "ns",
			IdentityKind: "Deployment", IdentityName: "api",
			Containers: []corev1.Container{{Name: "main"}},
		},
		{
			Kind: "Pod", Name: "dag-task", Namespace: "ns",
			IdentityKind: "Pod", IdentityName: "dag-task",
			Containers: []corev1.Container{{Name: "worker"}},
		},
	}

	idx, failures := r.discover(context.Background(), policy, targets)
	if failures != 0 {
		t.Fatalf("discover reported %d failures, want 0", failures)
	}

	if len(idx) != 2 {
		t.Fatalf("index has %d entries, want 2", len(idx))
	}
	for _, tc := range []struct{ kind, name string }{{"Deployment", "api"}, {"Pod", "dag-task"}} {
		var got sustainv1alpha1.WorkloadRecommendation
		key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name(tc.kind, tc.name)}
		if err := c.Get(context.Background(), key, &got); err != nil {
			t.Errorf("no WLR for %s/%s: %v", tc.kind, tc.name, err)
			continue
		}
		// A WLR must exist even though nothing has been computed. This is the
		// inversion the whole design rests on.
		if len(got.Status.Containers) != 0 {
			t.Errorf("%s/%s: containers = %d, want 0", tc.kind, tc.name, len(got.Status.Containers))
		}
	}
}

// When EnsureExists fails the identity is still computed and applied, but its
// recommendation has nowhere to be cached. A persistent cause (missing RBAC, a
// rejecting admission webhook, a quota) must not leave the Policy reporting
// Ready while the cache silently goes stale.
func TestReconcileSurfacesDiscoveryFailures(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing}},
			},
		},
	}

	server := promServerForReconcile(t)
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	// Every WorkloadRecommendation create is rejected, as a missing RBAC rule
	// on the resource would do.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(policy, annotatedDeployment("default", "web", "p")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: "k8s.sustain.io", Resource: "workloadrecommendations"},
						obj.GetName(), errors.New("no RBAC"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		QueryShardMaxSamples:     10_000_000,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, true),
		retries:                  newRetryTracker(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	if ready == nil {
		t.Fatal("no Ready condition written")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False: an identity that could not be registered must not report success", ready.Status)
	}
	if !strings.Contains(ready.Message, "could not be registered") {
		t.Errorf("message = %q, want it to name the registration failure", ready.Message)
	}
}

// Members of an owner-name group share ONE WorkloadRecommendation, so writing
// the snapshot once per TARGET makes them overwrite each other's
// status.observedResources every cycle, forever, with the survivor decided by
// listing order. One write per identity, from an order-independent snapshot,
// and no write at all on a second cycle that changed nothing.
func TestDiscoverWritesOneStableSnapshotPerIdentity(t *testing.T) {
	statusPatches := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					statusPatches++
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	// Same identity, deliberately DIFFERENT container resources — mid-migration
	// blue/green, where the two members are not (yet) sized alike. Green also
	// carries a container blue does not, so the union matters.
	targets := []workloadTarget{
		{
			Kind: "Deployment", Name: "api-blue", Namespace: "ns",
			IdentityKind: "Deployment", IdentityName: "api",
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			}},
		},
		{
			Kind: "Deployment", Name: "api-green", Namespace: "ns",
			IdentityKind: "Deployment", IdentityName: "api",
			Containers: []corev1.Container{
				{
					Name:      "main",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}},
				},
				{Name: "sidecar"},
			},
		},
	}

	if _, failures := r.discover(context.Background(), policy, targets); failures != 0 {
		t.Fatalf("discover reported %d failures, want 0", failures)
	}
	if statusPatches != 1 {
		t.Errorf("first cycle issued %d status writes, want 1: one identity is one snapshot, "+
			"not one per group member", statusPatches)
	}

	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Deployment", "api")}
	var first sustainv1alpha1.WorkloadRecommendation
	if err := c.Get(context.Background(), key, &first); err != nil {
		t.Fatalf("get WLR: %v", err)
	}
	if len(first.Status.ObservedResources) != 2 {
		t.Errorf("snapshot covers %d containers, want 2 (the union of the group's members): "+
			"a container only one member declares would otherwise be missing from the recommendation "+
			"and from the shard sizing", len(first.Status.ObservedResources))
	}

	// Second cycle, same input in the REVERSE listing order — the API server
	// returns no ordering guarantee across cycles. Nothing changed, so nothing
	// may be written.
	statusPatches = 0
	reversed := []workloadTarget{targets[1], targets[0]}
	if _, failures := r.discover(context.Background(), policy, reversed); failures != 0 {
		t.Fatalf("second discover reported %d failures, want 0", failures)
	}
	if statusPatches != 0 {
		t.Errorf("second cycle issued %d status writes, want 0: group members must not flap "+
			"each other's snapshot every reconcile", statusPatches)
	}

	var second sustainv1alpha1.WorkloadRecommendation
	if err := c.Get(context.Background(), key, &second); err != nil {
		t.Fatalf("get WLR after second cycle: %v", err)
	}
	if !reflect.DeepEqual(first.Status.ObservedResources, second.Status.ObservedResources) {
		t.Errorf("snapshot changed across cycles:\nfirst  = %v\nsecond = %v",
			first.Status.ObservedResources, second.Status.ObservedResources)
	}
}

func TestDiscoverIndexesByIdentityNotObjectName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	// Two objects grouped under one owner-name identity.
	targets := []workloadTarget{
		{
			Kind: "Deployment", Name: "api-blue", Namespace: "ns",
			IdentityKind: "Deployment", IdentityName: "api",
			Containers: []corev1.Container{{Name: "main"}},
		},
		{
			Kind: "Deployment", Name: "api-green", Namespace: "ns",
			IdentityKind: "Deployment", IdentityName: "api",
			Containers: []corev1.Container{{Name: "main"}},
		},
	}

	idx, failures := r.discover(context.Background(), policy, targets)
	if failures != 0 {
		t.Fatalf("discover reported %d failures, want 0", failures)
	}

	if len(idx) != 1 {
		t.Fatalf("index has %d entries, want 1: grouped objects share one identity", len(idx))
	}
	// One identity, but BOTH objects retained: application is per real
	// workload object, so dropping a member here would silently stop
	// recycling its pods forever.
	id := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "api"}
	if got := len(idx[id]); got != 2 {
		t.Errorf("identity holds %d targets, want 2: every group member must stay reachable by the apply phase", got)
	}
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("created %d WLRs, want 1", len(list.Items))
	}
}
