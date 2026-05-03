package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestWlrName(t *testing.T) {
	if got := wlrName("Deployment", "web"); got != "deployment-web" {
		t.Errorf("wlrName Deployment/web = %q, want deployment-web", got)
	}
	if got := wlrName("StatefulSet", "db"); got != "statefulset-db" {
		t.Errorf("wlrName StatefulSet/db = %q", got)
	}
}

func TestQuantityPtrEqual(t *testing.T) {
	q := resource.MustParse("100m")
	other := resource.MustParse("200m")

	if !quantityPtrEqual(nil, nil) {
		t.Error("nil/nil should be equal")
	}
	if quantityPtrEqual(&q, nil) {
		t.Error("nonzero/nil should not be equal")
	}
	if !quantityPtrEqual(&q, &q) {
		t.Error("same value should be equal")
	}
	if quantityPtrEqual(&q, &other) {
		t.Error("100m/200m should not be equal")
	}
}

// reconcilerForCache builds a PolicyReconciler with WLR scheme registered.
func reconcilerForCache(t *testing.T, objs ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...).
		Build()
	return &PolicyReconciler{Client: c, Scheme: scheme}
}

// TestUpsertWorkloadRecommendation_CreatesObjectOnFirstCall verifies the
// controller creates a new WLR when one doesn't exist for a workload.
func TestUpsertWorkloadRecommendation_CreatesObjectOnFirstCall(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	now := metav1.Now()

	r.upsertWorkloadRecommendation(context.Background(),
		&workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web"},
		"p",
		map[string]workload.ContainerRecommendation{
			"app": {CPURequest: &cpu, MemoryRequest: &mem},
		},
		now,
	)

	var got sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &got); err != nil {
		t.Fatalf("expected WLR to exist after upsert: %v", err)
	}
	if got.Spec.WorkloadRef.Kind != "Deployment" || got.Spec.WorkloadRef.Name != "web" {
		t.Errorf("workload ref wrong: %+v", got.Spec.WorkloadRef)
	}
	if got.Spec.Policy != "p" {
		t.Errorf("policy = %q, want p", got.Spec.Policy)
	}
	if got.Status.ObservedAt.IsZero() {
		t.Error("ObservedAt not stamped")
	}
	if got.Status.Source != "prometheus" {
		t.Errorf("source = %q, want prometheus", got.Status.Source)
	}
	if c := got.Status.Containers["app"]; c.CPURequest == nil || c.CPURequest.Cmp(cpu) != 0 {
		t.Errorf("container cpu mismatch: %v", c.CPURequest)
	}
}

// TestUpsertWorkloadRecommendation_NoOpWhenUnchanged verifies that calling
// upsert twice with the same recommendation does NOT bump the resourceVersion
// — the compare-before-write guard skips the etcd round-trip.
func TestUpsertWorkloadRecommendation_NoOpWhenUnchanged(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	tgt := &workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web"}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: &cpu, MemoryRequest: &mem},
	}

	r.upsertWorkloadRecommendation(context.Background(), tgt, "p", recs, metav1.Now())
	var first sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &first); err != nil {
		t.Fatalf("get: %v", err)
	}
	rvBefore := first.ResourceVersion

	r.upsertWorkloadRecommendation(context.Background(), tgt, "p", recs, metav1.Now())
	var second sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &second); err != nil {
		t.Fatalf("get: %v", err)
	}
	if second.ResourceVersion != rvBefore {
		t.Errorf("expected no etcd write on identical recommendation, resourceVersion bumped from %s to %s", rvBefore, second.ResourceVersion)
	}
}

// TestUpsertWorkloadRecommendation_UpdatesOnChange verifies that a different
// recommendation triggers a status patch.
func TestUpsertWorkloadRecommendation_UpdatesOnChange(t *testing.T) {
	r := reconcilerForCache(t)
	cpu1 := resource.MustParse("250m")
	cpu2 := resource.MustParse("500m")
	tgt := &workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web"}

	r.upsertWorkloadRecommendation(context.Background(), tgt, "p",
		map[string]workload.ContainerRecommendation{"app": {CPURequest: &cpu1}}, metav1.Now())

	r.upsertWorkloadRecommendation(context.Background(), tgt, "p",
		map[string]workload.ContainerRecommendation{"app": {CPURequest: &cpu2}}, metav1.Now())

	var got sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Containers["app"].CPURequest.Cmp(cpu2) != 0 {
		t.Errorf("expected updated cpu=500m, got %v", got.Status.Containers["app"].CPURequest)
	}
}

// TestSweepWorkloadRecommendations_RemovesOrphans verifies the sweeper deletes
// WLRs whose target workload is no longer in the policy's matched set.
func TestSweepWorkloadRecommendations_RemovesOrphans(t *testing.T) {
	live := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-live"},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "p",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "live"},
		},
	}
	orphan := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-orphan"},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "p",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "orphan"},
		},
	}
	otherPolicy := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-foreign"},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "other",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "foreign"},
		},
	}
	r := reconcilerForCache(t, live, orphan, otherPolicy)

	targets := []workloadTarget{{Kind: "Deployment", Namespace: "default", Name: "live"}}
	r.sweepWorkloadRecommendations(context.Background(), "p", targets)

	// live: present
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-live"}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("live entry should remain, got error: %v", err)
	}
	// orphan: deleted
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-orphan"}, &sustainv1alpha1.WorkloadRecommendation{})
	if err == nil {
		t.Error("orphan WLR should have been deleted")
	}
	// other-policy: untouched
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-foreign"}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("foreign-policy entry should remain, got error: %v", err)
	}
}

// TestStatusEquivalent_DistinguishesSameValuesFromDifferentSources verifies
// the equivalence check ignores ObservedAt but respects Source.
func TestStatusEquivalent_DistinguishesSameValuesFromDifferentSources(t *testing.T) {
	cpu := resource.MustParse("250m")
	now := metav1.NewTime(time.Now())
	later := metav1.NewTime(now.Add(time.Minute))

	a := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt: now,
		Source:     "prometheus",
		Containers: map[string]sustainv1alpha1.ContainerRecommendation{
			"app": {CPURequest: &cpu},
		},
	}
	b := a
	b.ObservedAt = later
	if !statusEquivalent(a, b) {
		t.Error("differ only by ObservedAt → should be equivalent")
	}

	c := a
	c.Source = "fallback"
	if statusEquivalent(a, c) {
		t.Error("different Source → should NOT be equivalent")
	}
}

// TestDeleteAllRecommendationsForPolicy_DeletesAllForPolicy verifies the
// strategy-1 cleanup path (called from the deletion branch of Reconcile)
// removes every WLR for the named policy and leaves other policies' WLRs
// untouched.
func TestDeleteAllRecommendationsForPolicy_DeletesAllForPolicy(t *testing.T) {
	mine := []*sustainv1alpha1.WorkloadRecommendation{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-a"},
			Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-b"},
			Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
		},
	}
	other := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-c"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "other"},
	}
	objs := []runtime.Object{mine[0], mine[1], other}
	r := reconcilerForCache(t, objs...)

	if err := r.deleteAllRecommendationsForPolicy(context.Background(), "p"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, w := range mine {
		err := r.Get(context.Background(), types.NamespacedName{Namespace: w.Namespace, Name: w.Name}, &sustainv1alpha1.WorkloadRecommendation{})
		if err == nil {
			t.Errorf("expected %s to be deleted", w.Name)
		}
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: other.Namespace, Name: other.Name}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("other-policy WLR should remain, got error: %v", err)
	}
}

// TestReapOrphanedRecommendations_DeletesOnlyOrphans verifies the strategy-2
// periodic sweep: WLRs whose policy still exists are kept; WLRs referencing
// a vanished policy are deleted; entries with empty spec.policy are left
// alone.
func TestReapOrphanedRecommendations_DeletesOnlyOrphans(t *testing.T) {
	livePolicy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "live"}}
	live := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-live"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "live"},
	}
	orphan := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-orphan"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "ghost"},
	}
	untracked := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-untracked"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: ""},
	}
	r := reconcilerForCache(t, livePolicy, live, orphan, untracked)

	if err := r.reapOrphanedRecommendations(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-live"}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("live entry should remain, got error: %v", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-orphan"}, &sustainv1alpha1.WorkloadRecommendation{}); err == nil {
		t.Error("orphan entry should have been reaped")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-untracked"}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("untracked entry (empty policy) should remain, got error: %v", err)
	}
}

// TestReconcile_PolicyDeletion_RemovesItsRecommendations is an end-to-end
// check that the strategy-1 hook fires on deletion: a Policy with associated
// WLRs is deleted, after which no WLRs remain for that policy.
func TestReconcile_PolicyDeletion_RemovesItsRecommendations(t *testing.T) {
	now := metav1.Now()
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p",
			Finalizers:        []string{"k8s.sustain.io/cleanup"},
			DeletionTimestamp: &now,
		},
	}
	mine := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-a"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
	}
	other := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-c"},
		Spec:       sustainv1alpha1.WorkloadRecommendationSpec{Policy: "other"},
	}

	r, server := reconcilerForPolicy(t, policy, mine, other)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-a"}, &sustainv1alpha1.WorkloadRecommendation{}); err == nil {
		t.Error("WLR for deleted policy should have been removed")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-c"}, &sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("WLR for other policy should remain, got error: %v", err)
	}
}
