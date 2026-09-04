package wlrcache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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

func TestEnsureExistsCreatesEmptyWLR(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	ref := sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"}
	observed := map[string]sustainv1alpha1.ObservedContainerResources{
		"main": {Init: false},
	}

	if err := wlrcache.EnsureExists(context.Background(), c, ref, "pol", observed); err != nil {
		t.Fatalf("EnsureExists: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Policy != "pol" {
		t.Errorf("spec.policy = %q, want %q", got.Spec.Policy, "pol")
	}
	if got.Labels[sustainv1alpha1.WLRPolicyLabel] != "pol" {
		t.Errorf("policy label = %q, want %q", got.Labels[sustainv1alpha1.WLRPolicyLabel], "pol")
	}
	if _, ok := got.Status.ObservedResources["main"]; !ok {
		t.Error("observedResources not written")
	}
	if len(got.Status.Containers) != 0 {
		t.Errorf("containers = %d, want 0", len(got.Status.Containers))
	}
	if !got.Status.ObservedAt.IsZero() {
		t.Error("ObservedAt must stay zero: nothing has been computed yet")
	}
}

func TestEnsureExistsClearsDeparted(t *testing.T) {
	ref := sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"}
	// Rfc3339Copy: the fake client's tracker truncates metav1.Time to second
	// precision, same as a real apiserver.
	observedAt := metav1.NewTime(time.Now().Add(-30 * time.Minute)).Rfc3339Copy()
	q := resource.MustParse("250m")
	existing := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Pod", "dag-task"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: "pol"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Departed:   true,
			ObservedAt: observedAt,
			Source:     sustainv1alpha1.RecommendationSourcePrometheus,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"main": {CPURequest: &q},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(existing).Build()

	if err := wlrcache.EnsureExists(context.Background(), c, ref, "pol", nil); err != nil {
		t.Fatalf("EnsureExists: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Departed {
		t.Error("Departed must be cleared: the identity is in the target listing again")
	}
	// The realistic path for "never writes Containers, Source or ObservedAt":
	// in TestEnsureExistsCreatesEmptyWLR those fields are zero by construction
	// and the assertion would be vacuous.
	if len(got.Status.Containers) != 1 {
		t.Fatalf("containers = %d, want 1: EnsureExists must not touch Containers", len(got.Status.Containers))
	}
	if got.Status.Containers["main"].CPURequest.Cmp(q) != 0 {
		t.Errorf("containers[main].cpuRequest = %v, want unchanged %v", got.Status.Containers["main"].CPURequest, q)
	}
	if got.Status.Source != sustainv1alpha1.RecommendationSourcePrometheus {
		t.Errorf("source = %q, want unchanged %q", got.Status.Source, sustainv1alpha1.RecommendationSourcePrometheus)
	}
	if !got.Status.ObservedAt.Equal(&observedAt) {
		t.Errorf("observedAt = %v, want unchanged %v: EnsureExists must not stamp freshness", got.Status.ObservedAt, observedAt)
	}
}

func TestMarkNoDataLeavesPopulatedStatusAlone(t *testing.T) {
	ref := sustainv1alpha1.WorkloadReference{Kind: "Job", Namespace: "ns", Name: "nightly"}
	// Rfc3339Copy: the fake tracker serializes metav1.Time at second precision
	// (as a real apiserver does), so a sub-second value would fail the compare
	// for that reason alone.
	observedAt := metav1.NewTime(time.Now().Add(-2 * time.Hour)).Rfc3339Copy()
	q := resource.MustParse("100m")
	existing := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Job", "nightly"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: "pol"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: observedAt,
			Source:     sustainv1alpha1.RecommendationSourcePrometheus,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"main": {CPURequest: &q},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(existing).Build()

	if err := wlrcache.MarkNoData(context.Background(), c, ref, metav1.Now()); err != nil {
		t.Fatalf("MarkNoData: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Job", "nightly")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Containers) != 1 {
		t.Fatalf("containers = %d, want 1: a good recommendation must never be wiped", len(got.Status.Containers))
	}
	if !got.Status.ObservedAt.Equal(&observedAt) {
		t.Error("ObservedAt bumped: that would tell the webhook stale data is fresh")
	}
	if got.Status.Source != sustainv1alpha1.RecommendationSourcePrometheus {
		t.Errorf("source = %q, want unchanged", got.Status.Source)
	}
}

func TestMarkNoDataWritesWhenNeverPopulated(t *testing.T) {
	ref := sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "fresh"}
	existing := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Pod", "fresh"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: "pol"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(existing).Build()

	now := metav1.Now()
	if err := wlrcache.MarkNoData(context.Background(), c, ref, now); err != nil {
		t.Fatalf("MarkNoData: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "fresh")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Source != sustainv1alpha1.RecommendationSourceNoData {
		t.Errorf("source = %q, want %q", got.Status.Source, sustainv1alpha1.RecommendationSourceNoData)
	}
	if got.Status.ObservedAt.IsZero() {
		t.Error("ObservedAt not set: the reaper needs something to age against")
	}
}

// laggingCacheReader models the one thing fake.NewClientBuilder cannot: a
// cache-backed client is NOT read-your-writes, so a Get straight after a Create
// races the watch event and returns NotFound. It fails the first Get of each
// created-but-unwarmed key and succeeds for every other read.
//
// Without this the bug class is invisible: the pre-fix code re-read after
// Create and shipped, because every test it had used a read-your-writes fake.
type laggingCacheReader struct {
	mu   sync.Mutex
	cold map[string]struct{}
}

func newLaggingCacheReader() *laggingCacheReader {
	return &laggingCacheReader{cold: map[string]struct{}{}}
}

func (l *laggingCacheReader) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			l.mu.Lock()
			l.cold[obj.GetNamespace()+"/"+obj.GetName()] = struct{}{}
			l.mu.Unlock()
			return nil
		},
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			l.mu.Lock()
			_, cold := l.cold[key.String()]
			l.mu.Unlock()
			if cold {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: sustainv1alpha1.GroupVersion.Group, Resource: "workloadrecommendations"},
					key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

// warm models the watch event landing, so assertions read the persisted object
// rather than the lag.
func (l *laggingCacheReader) warm() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cold = map[string]struct{}{}
}

// A create is only half the write: the status subresource discards whatever
// Create carried, and re-reading to get an object to patch 404s off the cold
// cache — leaving an empty status.observedResources that the computation phase
// silently skips forever.
func TestEnsureExistsWritesSnapshotWhenTheCacheLagsBehindTheCreate(t *testing.T) {
	lag := newLaggingCacheReader()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(lag.funcs()).Build()

	ref := sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"}
	observed := map[string]sustainv1alpha1.ObservedContainerResources{"main": {Init: false}}

	if err := wlrcache.EnsureExists(context.Background(), c, ref, "pol", observed); err != nil {
		t.Fatalf("EnsureExists must not depend on reading back its own create: %v", err)
	}

	lag.warm()
	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := got.Status.ObservedResources["main"]; !ok {
		t.Fatalf("observedResources empty after create: computation will skip this identity forever, got %+v", got.Status)
	}
}

// Same hazard on the other writer. Upsert creating a WLR and then failing to
// write its status means the departed-refresh path reports a write it never
// made.
func TestUpsertWritesStatusWhenTheCacheLagsBehindTheCreate(t *testing.T) {
	lag := newLaggingCacheReader()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(lag.funcs()).Build()

	ref := sustainv1alpha1.WorkloadReference{Kind: "Job", Namespace: "ns", Name: "nightly"}
	cpu := resource.MustParse("250m")
	recs := map[string]workload.ContainerRecommendation{"main": {CPURequest: &cpu}}

	if err := wlrcache.Upsert(context.Background(), c, ref, "pol", recs, nil, metav1.Now()); err != nil {
		t.Fatalf("Upsert must not depend on reading back its own create: %v", err)
	}

	lag.warm()
	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Job", "nightly")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Containers) != 1 {
		t.Fatalf("containers = %d, want 1: the recommendation was never written, only the empty shell", len(got.Status.Containers))
	}
	if got.Status.Containers["main"].CPURequest.Cmp(cpu) != 0 {
		t.Errorf("containers[main].cpuRequest = %v, want %v", got.Status.Containers["main"].CPURequest, cpu)
	}
}
