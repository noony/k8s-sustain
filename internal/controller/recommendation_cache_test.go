package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// TestWlrName_LongNameTruncatedWithHash verifies names exceeding the 253-char
// object-name limit are truncated with a stable hash suffix. The expected
// literal is duplicated in the webhook package test — both copies of wlrName
// must produce it, or controller and webhook disagree on the cache key.
func TestWlrName_LongNameTruncatedWithHash(t *testing.T) {
	want := "deployment-" + strings.Repeat("a", 231) + "-55335e7810"
	got := wlrName("Deployment", strings.Repeat("a", 260))
	if got != want {
		t.Errorf("wlrName long input = %q, want %q", got, want)
	}
	if len(got) > 253 {
		t.Errorf("wlrName long input length = %d, want <= 253", len(got))
	}
}

// TestUpsertWorkloadRecommendation_UsesIdentityOverride verifies that when a
// target's IdentityKind/IdentityName differ from its real Kind/Name (the
// owner-name grouping override), the WorkloadRecommendation is named and
// spec'd using the override, not the real object identity.
func TestUpsertWorkloadRecommendation_UsesIdentityOverride(t *testing.T) {
	r := reconcilerForCache(t)
	target := &workloadTarget{
		Kind: "Deployment", Name: "app-blue", Namespace: "prod",
		IdentityKind: "Deployment", IdentityName: "app",
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qtyp("100m"), MemoryRequest: qtyp("64Mi")},
	}
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(target), "my-policy", recs, metav1.Now())

	var wlr sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: "deployment-app"}
	if err := r.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("expected WorkloadRecommendation %v to exist, got: %v", key, err)
	}
	if wlr.Spec.WorkloadRef.Kind != "Deployment" || wlr.Spec.WorkloadRef.Name != "app" {
		t.Errorf("WorkloadRef = %s/%s, want Deployment/app (overridden identity)",
			wlr.Spec.WorkloadRef.Kind, wlr.Spec.WorkloadRef.Name)
	}

	var notWanted sustainv1alpha1.WorkloadRecommendation
	notWantedKey := types.NamespacedName{Namespace: "prod", Name: "deployment-app-blue"}
	if err := r.Get(context.Background(), notWantedKey, &notWanted); err == nil {
		t.Errorf("did not expect a WorkloadRecommendation keyed by the real name %v", notWantedKey)
	}
}

// qtyp parses a resource.Quantity string and returns a pointer to it, for
// building ContainerRecommendation literals in tests.
func qtyp(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// reconcilerForCache builds a PolicyReconciler with WLR scheme registered.
func reconcilerForCache(t *testing.T, objs ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
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

// wlrFor builds a WorkloadRecommendation labeled for policyName whose target
// is (kind, ns, name) and whose recommendation was last observed at observedAt.
func wlrFor(policyName, ns, kind, name string, observedAt time.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      wlrName(kind, name),
			Labels:    map[string]string{wlrPolicyLabel: policyName},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: ns, Name: name},
			Policy:      policyName,
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(observedAt),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"main": {CPURequest: qtyp("100m")}},
		},
	}
}

// getWLRFor reads back the WLR named for (kind, name) in ns, failing the test
// if it is absent. Used where the assertion is about status contents rather
// than mere survival.
func getWLRFor(t *testing.T, r *PolicyReconciler, ns, kind, name string) *sustainv1alpha1.WorkloadRecommendation {
	t.Helper()
	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: wlrName(kind, name)}, &wlr); err != nil {
		t.Fatalf("get WLR %s/%s: %v", ns, wlrName(kind, name), err)
	}
	return &wlr
}

// failingWorkloadGetClient fails Get for workload objects while leaving
// WorkloadRecommendation reads working, so retainDepartedWLR's existence check
// takes its error (fail-open) branch while the sweep otherwise runs normally.
type failingWorkloadGetClient struct {
	client.Client
}

func (c failingWorkloadGetClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	return errors.New("simulated apiserver failure on workload existence check")
}

// wlrExists reports whether the WLR named for (kind, name) in ns survives.
func wlrExists(t *testing.T, r *PolicyReconciler, ns, kind, name string) bool {
	t.Helper()
	var wlr sustainv1alpha1.WorkloadRecommendation
	err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: wlrName(kind, name)}, &wlr)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get WLR: %v", err)
	}
	return err == nil
}

// TestUpsertWorkloadRecommendation_CreatesObjectOnFirstCall verifies the
// controller creates a new WLR when one doesn't exist for a workload.
func TestUpsertWorkloadRecommendation_CreatesObjectOnFirstCall(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	now := metav1.Now()

	_ = r.upsertWorkloadRecommendation(context.Background(),
		itemForTarget(&workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}),
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

// TestUpsertWorkloadRecommendation_PersistsRemoveFlags verifies that the
// NoLimit intent (RemoveCPULimit / RemoveMemoryLimit) is persisted on the
// status. The webhook reads these on Prometheus outage — losing them silently
// reverts NoLimit policies to "leave template alone" during outages.
func TestUpsertWorkloadRecommendation_PersistsRemoveFlags(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")

	_ = r.upsertWorkloadRecommendation(context.Background(),
		itemForTarget(&workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}),
		"p",
		map[string]workload.ContainerRecommendation{
			"app": {
				CPURequest:        &cpu,
				MemoryRequest:     &mem,
				RemoveCPULimit:    true,
				RemoveMemoryLimit: true,
			},
		},
		metav1.Now(),
	)

	var got sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &got); err != nil {
		t.Fatalf("expected WLR to exist after upsert: %v", err)
	}
	c := got.Status.Containers["app"]
	if !c.RemoveCPULimit {
		t.Error("RemoveCPULimit not persisted on WorkloadRecommendation status")
	}
	if !c.RemoveMemoryLimit {
		t.Error("RemoveMemoryLimit not persisted on WorkloadRecommendation status")
	}
}

// TestUpsertWorkloadRecommendation_NoOpWhenUnchanged verifies that calling
// upsert twice with the same recommendation does NOT bump the resourceVersion
// — the compare-before-write guard skips the etcd round-trip.
func TestUpsertWorkloadRecommendation_NoOpWhenUnchanged(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("128Mi")
	tgt := &workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: &cpu, MemoryRequest: &mem},
	}

	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p", recs, metav1.Now())
	var first sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &first); err != nil {
		t.Fatalf("get: %v", err)
	}
	rvBefore := first.ResourceVersion

	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p", recs, metav1.Now())
	var second sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &second); err != nil {
		t.Fatalf("get: %v", err)
	}
	if second.ResourceVersion != rvBefore {
		t.Errorf("expected no etcd write on identical recommendation, resourceVersion bumped from %s to %s", rvBefore, second.ResourceVersion)
	}
}

// TestUpsertWorkloadRecommendation_RefreshesStaleObservedAt verifies that an
// equivalent recommendation still triggers a status write once ObservedAt is
// older than wlrRefreshInterval — otherwise stable workloads would freeze
// ObservedAt and the webhook would reject the cache as stale (>30m) exactly
// when the Prometheus-outage fallback is needed.
func TestUpsertWorkloadRecommendation_RefreshesStaleObservedAt(t *testing.T) {
	r := reconcilerForCache(t)
	cpu := resource.MustParse("250m")
	tgt := &workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}
	recs := map[string]workload.ContainerRecommendation{"app": {CPURequest: &cpu}}

	past := metav1.NewTime(time.Now().Add(-2 * wlrRefreshInterval))
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p", recs, past)

	now := metav1.Now()
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p", recs, now)

	var got sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Status.ObservedAt.After(past.Time) {
		t.Errorf("expected ObservedAt refreshed past %v, got %v", past, got.Status.ObservedAt)
	}
}

// TestUpsertWorkloadRecommendation_UpdatesOnChange verifies that a different
// recommendation triggers a status patch.
func TestUpsertWorkloadRecommendation_UpdatesOnChange(t *testing.T) {
	r := reconcilerForCache(t)
	cpu1 := resource.MustParse("250m")
	cpu2 := resource.MustParse("500m")
	tgt := &workloadTarget{Kind: "Deployment", Namespace: "default", Name: "web", IdentityKind: "Deployment", IdentityName: "web"}

	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p",
		map[string]workload.ContainerRecommendation{"app": {CPURequest: &cpu1}}, metav1.Now())

	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(tgt), "p",
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
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-live",
			Labels: map[string]string{wlrPolicyLabel: "p"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "p",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "live"},
		},
	}
	orphan := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-orphan",
			Labels: map[string]string{wlrPolicyLabel: "p"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "p",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "orphan"},
		},
	}
	otherPolicy := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-foreign",
			Labels: map[string]string{wlrPolicyLabel: "other"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "other",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "foreign"},
		},
	}
	r := reconcilerForCache(t, live, orphan, otherPolicy)

	targets := []workloadTarget{{Kind: "Deployment", Namespace: "default", Name: "live", IdentityKind: "Deployment", IdentityName: "live"}}
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

// TestSweepWorkloadRecommendations_KeepsOverriddenIdentitySharedByTwoTargets
// verifies sweep does not delete a WorkloadRecommendation that two different
// real targets (e.g. app-blue and app-green) both write to via the same
// owner-name override — sweep's "wanted" set must be keyed by the override
// identity, not each target's real identity.
func TestSweepWorkloadRecommendations_KeepsOverriddenIdentitySharedByTwoTargets(t *testing.T) {
	r := reconcilerForCache(t)
	targets := []workloadTarget{
		{Kind: "Deployment", Name: "app-blue", Namespace: "prod", IdentityKind: "Deployment", IdentityName: "app", PolicyName: "my-policy"},
		{Kind: "Deployment", Name: "app-green", Namespace: "prod", IdentityKind: "Deployment", IdentityName: "app", PolicyName: "my-policy"},
	}
	recs := map[string]workload.ContainerRecommendation{"app": {CPURequest: qtyp("100m")}}
	for i := range targets {
		_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(&targets[i]), "my-policy", recs, metav1.Now())
	}

	r.sweepWorkloadRecommendations(context.Background(), "my-policy", targets)

	var wlr sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: "deployment-app"}
	if err := r.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("expected shared WorkloadRecommendation %v to survive sweep, got: %v", key, err)
	}
}

// TestDeleteAllRecommendationsForPolicy_DeletesAllForPolicy verifies the
// strategy-1 cleanup path (called from the deletion branch of Reconcile)
// removes every WLR for the named policy and leaves other policies' WLRs
// untouched.
func TestDeleteAllRecommendationsForPolicy_DeletesAllForPolicy(t *testing.T) {
	mine := []*sustainv1alpha1.WorkloadRecommendation{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "deployment-a",
				Labels: map[string]string{wlrPolicyLabel: "p"},
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "deployment-b",
				Labels: map[string]string{wlrPolicyLabel: "p"},
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
		},
	}
	other := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-c",
			Labels: map[string]string{wlrPolicyLabel: "other"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: "other"},
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

// TestReapKeepsNoDataRecommendations pins the reaper's single remaining rule:
// it collects orphans and nothing else. "nodata" used to be a terminal state
// aged out after 24h, which was the only thing that ever gave such an identity
// another attempt. Under WLR-driven refresh nodata means "nothing computed
// YET" and the computation phase retries it every cycle, so deleting one on
// age would throw away the observed-resources snapshot that keeps the identity
// in the work-list — turning a self-healing state back into a cold start.
func TestReapKeepsNoDataRecommendations(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p1"}}

	fresh := noDataStub("prod", "job-fresh", "p1", metav1.NewTime(time.Now().Add(-1*time.Hour)))
	ancient := noDataStub("prod", "job-ancient", "p1", metav1.NewTime(time.Now().Add(-1000*time.Hour)))
	unstamped := noDataStub("prod", "job-unstamped", "p1", metav1.Time{})
	orphan := noDataStub("prod", "job-orphan", "ghost", metav1.NewTime(time.Now().Add(-1000*time.Hour)))

	r := reconcilerForCache(t, policy, fresh, ancient, unstamped, orphan)

	if err := r.reapOrphanedRecommendations(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertWLRExists(t, r, "prod", "job-fresh")
	assertWLRExists(t, r, "prod", "job-ancient")
	assertWLRExists(t, r, "prod", "job-unstamped")
	// Still an orphan: nodata does not exempt an object whose Policy is gone.
	assertWLRAbsent(t, r, "prod", "job-orphan")
}

func noDataStub(ns, name, policy string, observed metav1.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{wlrPolicyLabel: policy},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: policy},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Source:     sustainv1alpha1.RecommendationSourceNoData,
			ObservedAt: observed,
		},
	}
}

func assertWLRExists(t *testing.T, r *PolicyReconciler, ns, name string) {
	t.Helper()
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name},
		&sustainv1alpha1.WorkloadRecommendation{}); err != nil {
		t.Errorf("%s/%s should still exist, got: %v", ns, name, err)
	}
}

func assertWLRAbsent(t *testing.T, r *PolicyReconciler, ns, name string) {
	t.Helper()
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name},
		&sustainv1alpha1.WorkloadRecommendation{}); err == nil {
		t.Errorf("%s/%s should have been reaped", ns, name)
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
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-a",
			Labels: map[string]string{wlrPolicyLabel: "p"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: "p"},
	}
	other := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deployment-c",
			Labels: map[string]string{wlrPolicyLabel: "other"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{Policy: "other"},
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

// TestUpsertWorkloadRecommendation_SnapshotsObservedResources verifies the
// upsert records what the containers actually ran with, including the init
// marker, so inactive dashboard rows can show current-vs-recommended after
// the workload object is gone.
func TestUpsertWorkloadRecommendation_SnapshotsObservedResources(t *testing.T) {
	r := reconcilerForCache(t)
	target := &workloadTarget{
		Kind: "Pod", Name: "etl", Namespace: "airflow",
		IdentityKind: "Pod", IdentityName: "etl",
		Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		}},
		InitContainers: []corev1.Container{{Name: "init-db"}},
	}
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(target), "p",
		map[string]workload.ContainerRecommendation{"main": {CPURequest: qtyp("250m")}}, metav1.Now())

	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "airflow", Name: "pod-etl"}, &wlr); err != nil {
		t.Fatalf("expected WLR to exist: %v", err)
	}
	obs := wlr.Status.ObservedResources
	if len(obs) != 2 {
		t.Fatalf("observedResources has %d entries, want 2 (main, init-db): %v", len(obs), obs)
	}
	main := obs["main"]
	if main.Init {
		t.Error("main wrongly marked Init")
	}
	if main.CPURequest == nil || main.CPURequest.String() != "500m" {
		t.Errorf("main.CPURequest = %v, want 500m", main.CPURequest)
	}
	if main.MemoryLimit == nil || main.MemoryLimit.String() != "1Gi" {
		t.Errorf("main.MemoryLimit = %v, want 1Gi", main.MemoryLimit)
	}
	if main.MemoryRequest != nil || main.CPULimit != nil {
		t.Errorf("unset fields must stay nil: %+v", main)
	}
	if !obs["init-db"].Init {
		t.Error("init-db must be marked Init")
	}
}

// TestUpsertWorkloadRecommendation_RewritesWhenObservedResourcesChange
// verifies that a change in the container's actual resources alone (same
// recommendation) still triggers a status write — the equivalence check must
// compare the snapshot, or inactive rows would show stale "current" values.
func TestUpsertWorkloadRecommendation_RewritesWhenObservedResourcesChange(t *testing.T) {
	r := reconcilerForCache(t)
	recs := map[string]workload.ContainerRecommendation{"main": {CPURequest: qtyp("250m")}}
	target := &workloadTarget{
		Kind: "Deployment", Name: "web", Namespace: "default",
		IdentityKind: "Deployment", IdentityName: "web",
		Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}},
	}
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(target), "p", recs, metav1.Now())

	target.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("750m")
	_ = r.upsertWorkloadRecommendation(context.Background(), itemForTarget(target), "p", recs, metav1.Now())

	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &wlr); err != nil {
		t.Fatalf("expected WLR: %v", err)
	}
	if got := wlr.Status.ObservedResources["main"].CPURequest; got == nil || got.String() != "750m" {
		t.Errorf("snapshot not refreshed: CPURequest = %v, want 750m", got)
	}
}

// TestSweep_RetainsDepartedWorkloadWithinRetention: the workload object is
// gone and the recommendation is within retention — the WLR must survive so
// the dashboard keeps showing the ephemeral workload.
func TestSweep_RetainsDepartedWorkloadWithinRetention(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR for departed workload deleted within retention window")
	}
}

// Retaining the object is only half of what a departed identity needs. Its
// ObservedAt stops advancing as soon as the recompute stops finding data — the
// samples age out of the query window and the write rules deliberately keep the
// last-known-good rather than bumping the timestamp — so the webhook would read
// it as stale and admit every subsequent run on template resources, for the
// entire retention window, with the recommendation sitting right there. The
// sweep therefore records the departure it just confirmed.
func TestSweep_MarksRetainedDepartedWorkload(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "nightly", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)

	wlr := getWLRFor(t, r, "ci", "Job", "nightly")
	if !wlr.Status.Departed {
		t.Error("a retained departed WLR must be marked Departed, or the webhook reads it as " +
			"stale and every run after the first starts on template resources")
	}
}

// The mark waives the webhook's freshness gate, so it must never be applied on
// a guess. retainDepartedWLR keeps the object when the existence check ERRORS
// too — that is a fail-open, not a confirmed departure, and marking there would
// let a workload that is very much alive be served arbitrarily old data the
// moment an apiserver call flakes.
func TestSweep_DoesNotMarkDepartedWhenExistenceCheckFails(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	base := r.Client
	r.Client = failingWorkloadGetClient{Client: base}
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	r.Client = base

	if wlr := getWLRFor(t, r, "prod", "Deployment", "web"); wlr.Status.Departed {
		t.Error("an inconclusive existence check must not mark the WLR departed: that would waive " +
			"the staleness gate for a workload that may still be running")
	}
}

// TestSweep_DeletesDepartedWorkloadPastRetention: recommendation older than
// the retention window — swept.
func TestSweep_DeletesDepartedWorkloadPastRetention(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-80*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR past retention window survived the sweep")
	}
}

// TestSweep_DeletesOptedOutWorkloadAfterGrace: the Deployment still exists
// but left the target set (annotation removed / policy unmatched) — retention
// must NOT apply once past the fresh-write grace.
func TestSweep_DeletesOptedOutWorkloadAfterGrace(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)), dep)
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("WLR for opted-out (still existing) workload must be deleted")
	}
}

// TestSweep_TerminalJobCountsAsGone: a Complete Job leaves the target set
// while its object lingers until TTL/hook deletion. It did not opt out, so
// its WLR must be retained.
func TestSweep_TerminalJobCountsAsGone(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "argocd-hook"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-1*time.Hour)), job)
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR for terminal-but-present Job must be retained")
	}
}

// TestSweep_BarePodIdentityAlwaysRetainedUntilExpiry: bare-pod refs carry the
// owner-name value, not a real object name, so existence can't be checked —
// they ride out the retention window unconditionally.
func TestSweep_BarePodIdentityAlwaysRetainedUntilExpiry(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "airflow", "Pod", "etl", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "airflow", "Pod", "etl") {
		t.Error("bare-pod WLR deleted within retention window")
	}
}

// TestSweep_ZeroRetentionSweepsDepartedAfterGrace: retention disabled —
// departed targets are swept once past the fresh-write grace (legacy
// behavior, delayed at most 10 minutes).
func TestSweep_ZeroRetentionSweepsDepartedAfterGrace(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 0
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("retention=0 must sweep departed WLRs")
	}
}

// TestSweep_GracePeriodProtectsFreshWrites: a WLR created moments ago (e.g.
// by the webhook for a pod created after this cycle's target listing) must
// never be swept — even with retention disabled and even when its workload
// exists but is missing from the (stale) target list.
//
// The freshness is expressed as a fresh CreationTimestamp on both objects,
// which is what "written moments ago" actually looks like in the API. It used
// to be expressed as a fresh status.ObservedAt with both CreationTimestamps
// left at the zero time — a fixture that cannot occur, and one that made this
// test pass for the wrong reason: ObservedAt is rewritten by the controller's
// own computation phase for identities that are NOT in the target set, so it
// cannot distinguish a fresh write from this pass's own refresh. See
// sweepGracePeriod.
func TestSweep_GracePeriodProtectsFreshWrites(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ci", Name: "argocd-hook",
		CreationTimestamp: metav1.Now(),
	}}
	wlr := wlrFor("p", "ci", "Job", "argocd-hook", time.Now())
	wlr.CreationTimestamp = metav1.Now()
	r := reconcilerForCache(t, wlr, job)
	r.RecommendationRetention = 0
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("fresh WLR swept within grace period")
	}
}

// TestSweep_GraceCoversFreshlyCreatedStatuslessWLR: the webhook Creates the
// WLR before patching its status — between those calls ObservedAt is zero.
// The grace period must key off CreationTimestamp too, or the sweep deletes
// the record mid-write and a bare pod's only admission is lost forever.
func TestSweep_GraceCoversFreshlyCreatedStatuslessWLR(t *testing.T) {
	wlr := wlrFor("p", "airflow", "Pod", "etl", time.Time{})
	wlr.Status = sustainv1alpha1.WorkloadRecommendationStatus{} // no status patch yet
	// CreationTimestamp is stamped by the fake client at Create time; set it
	// explicitly since WithRuntimeObjects bypasses Create defaulting.
	wlr.CreationTimestamp = metav1.Now()
	r := reconcilerForCache(t, wlr)
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "airflow", "Pod", "etl") {
		t.Error("status-less freshly created WLR swept mid-write; grace must cover the Create→status-Patch window")
	}
}

// TestSweep_KeepsWLROnExistenceCheckError: a transient GET error (here: a
// kind whose scheme/CRD isn't registered, e.g. Argo Rollouts not installed)
// must fail open — keep the WLR and let a later sweep decide.
func TestSweep_KeepsWLROnExistenceCheckError(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "prod", "Rollout", "canary", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	if !wlrExists(t, r, "prod", "Rollout", "canary") {
		t.Error("WLR deleted despite existence-check error; must fail open")
	}
}

// TestSweep_DeletesWLRForOptedOutWorkloadStillRunning is the regression test
// for the sweep's grace anchor.
//
// It must go through a full Reconcile rather than calling
// sweepWorkloadRecommendations directly: the bug was that phase 2 recomputed
// the now-unmatched identity and rewrote status.ObservedAt, and the sweep at
// the end of the SAME pass then read that timestamp as proof of freshness. A
// direct sweep call with a stale ObservedAt passes either way and proves
// nothing.
//
// Retention is disabled so nothing but the grace period could keep the object:
// the assertion is unambiguous.
func TestSweep_DeletesWLRForOptedOutWorkloadStillRunning(t *testing.T) {
	const ns = "optout"
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing}},
			},
		},
	}

	// Running, well past the grace period, but no longer carrying the policy
	// annotation: opted out.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "api",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:      "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")}},
				}}},
			},
		},
	}

	// Its WLR from when it was still matched: a populated snapshot, so the
	// computation phase has containers to compute against.
	wlr := wlrFor("p", ns, "Deployment", "api", time.Now().Add(-1*time.Hour))
	wlr.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
	wlr.Status.Containers = map[string]sustainv1alpha1.ContainerRecommendation{"app": {CPURequest: qtyp("100m")}}
	wlr.Status.ObservedResources = map[string]sustainv1alpha1.ObservedContainerResources{"app": {CPURequest: qtyp("10m")}}

	// Prometheus still serves samples for the identity — the workload is up.
	// This is what refreshed ObservedAt and made the grace period
	// self-satisfying.
	server := promServerFor(ns, "Deployment", "api")
	defer server.Close()

	r := reconcilerWithProm(t, server, true, policy, dep, wlr)
	r.RecommendationRetention = 0

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wlrExists(t, r, ns, "Deployment", "api") {
		t.Error("WLR for an opted-out but still-running workload survived the sweep; " +
			"the grace period must not be satisfied by this pass's own refresh write")
	}
}

// TestSweep_KeepsWLRForWorkloadYoungerThanGrace: a workload created after the
// cycle's target listing was built is absent from that listing through no
// fault of its own. Its WLR (already created, e.g. by the webhook admitting
// its first pod) must survive until the next listing picks the workload up.
func TestSweep_KeepsWLRForWorkloadYoungerThanGrace(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "prod", Name: "fresh",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * time.Second)),
	}}
	wlr := wlrFor("p", "prod", "Deployment", "fresh", time.Now().Add(-1*time.Hour))
	wlr.CreationTimestamp = metav1.NewTime(time.Now().Add(-1 * time.Hour))

	r := reconcilerForCache(t, dep, wlr)
	r.RecommendationRetention = 0 // must not matter for a live workload
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)

	if !wlrExists(t, r, "prod", "Deployment", "fresh") {
		t.Error("WLR deleted for a workload younger than the grace period; " +
			"it postdates the target listing rather than having opted out")
	}
}

// rewriteOnListClient simulates the window the sweep reads through: every
// WorkloadRecommendation List is answered from the store and then each listed
// object is rewritten (bumping its resourceVersion) before the caller gets a
// look at it. That is exactly what a second Reconcile does when a workload is
// re-annotated from policy P1 to P2 — P2 re-labels and rewrites the object
// while P1's sweep is still holding the copy it listed.
type rewriteOnListClient struct {
	client.Client
	rewrites int
}

func (c *rewriteOnListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	wlrs, ok := list.(*sustainv1alpha1.WorkloadRecommendationList)
	if !ok {
		return nil
	}
	for i := range wlrs.Items {
		fresh := wlrs.Items[i].DeepCopy()
		fresh.Labels[wlrPolicyLabel] = "p2"
		fresh.Spec.Policy = "p2"
		if err := c.Update(ctx, fresh); err != nil {
			return err
		}
		c.rewrites++
	}
	return nil
}

// TestSweep_DoesNotDeleteWLRRewrittenSinceItWasListed: the sweep decides on a
// copy read from the informer cache, so by the time it issues the Delete the
// object may already belong to another policy, carrying a freshly computed
// recommendation. Deleting it there destroys live state and leaves the webhook
// injecting template resources until the new policy's next cycle (up to
// --reconcile-interval). The Delete must therefore be conditioned on the
// resourceVersion that was observed, so a stale decision fails as a conflict
// instead.
func TestSweep_DoesNotDeleteWLRRewrittenSinceItWasListed(t *testing.T) {
	// Still running, well past the grace period, absent from the target set:
	// the "opted out" branch, which deletes unconditionally today.
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)), dep)
	r.RecommendationRetention = 72 * time.Hour

	base := r.Client
	racy := &rewriteOnListClient{Client: base}
	r.Client = racy
	r.sweepWorkloadRecommendations(context.Background(), "p", nil)
	r.Client = base

	if racy.rewrites == 0 {
		t.Fatal("test setup: the sweep never listed the WorkloadRecommendation")
	}
	if !wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("the sweep deleted a WorkloadRecommendation that had been rewritten since it was " +
			"listed; the delete must carry the observed resourceVersion as a precondition")
	}
}

// A conflicting delete is not a failure to report: the object changed under
// the sweep, which means some other reconcile is now responsible for it. The
// policy-deletion path must not fail (and so must not hold its finalizer) over
// one.
func TestDeleteAllRecommendationsForPolicy_ConflictIsBenign(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))

	base := r.Client
	racy := &rewriteOnListClient{Client: base}
	r.Client = racy
	err := r.deleteAllRecommendationsForPolicy(context.Background(), "p")
	r.Client = base

	if err != nil {
		t.Errorf("a delete that conflicted with a concurrent rewrite must be treated as benign, got %v", err)
	}
	if racy.rewrites == 0 {
		t.Fatal("test setup: the delete path never listed the WorkloadRecommendation")
	}
}

// The precondition must not break the ordinary case: an untouched WLR is still
// deleted, and a delete of an object someone else already removed still counts
// as done rather than as an error.
func TestDeleteWLRsWhere_DeletesUnchangedAndToleratesAlreadyGone(t *testing.T) {
	present := wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour))
	r := reconcilerForCache(t, present)

	deleted, listErr, deleteErr := r.deleteWLRsWhere(context.Background(), logr.Discard(), nil,
		func(*sustainv1alpha1.WorkloadRecommendation) bool { return false })
	if listErr != nil || deleteErr != nil {
		t.Fatalf("list err %v, delete err %v", listErr, deleteErr)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("an unchanged WLR must still be deleted")
	}
}
