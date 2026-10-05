package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

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
	if err := rolloutsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(objs...).
		Build()
	return &PolicyReconciler{Client: c, Scheme: scheme}
}

// snapshotAll takes the inventory of everything r's cluster holds, which is
// what the sweep judges the WorkloadRecommendations against.
func snapshotAll(t *testing.T, r *PolicyReconciler) *inventory.Snapshot {
	t.Helper()
	snap, err := inventory.Take(context.Background(), r.Client, inventory.Options{})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	return snap
}

// sweep runs policyName's sweep against a fresh snapshot of r's cluster.
func sweep(t *testing.T, r *PolicyReconciler, policyName string) {
	t.Helper()
	r.sweepWorkloadRecommendations(context.Background(), policyName, snapshotAll(t, r))
}

// governingPolicy manages every kind, so whatever opts into it is governed.
func governingPolicy(name string) *sustainv1alpha1.Policy {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	return ongoingPolicy(name, sustainv1alpha1.UpdateTypes{
		Deployment: &ongoing, StatefulSet: &ongoing, DaemonSet: &ongoing, ArgoRollout: &ongoing,
		CronJob: &ongoing, Job: &ongoing, Pod: &ongoing,
	})
}

// wlrFor builds a WorkloadRecommendation labeled for policyName whose target
// is (kind, ns, name) and whose recommendation was last observed at observedAt.
func wlrFor(policyName, ns, kind, name string, observedAt time.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      wlrcache.Name(kind, name),
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
		types.NamespacedName{Namespace: ns, Name: wlrcache.Name(kind, name)}, &wlr); err != nil {
		t.Fatalf("get WLR %s/%s: %v", ns, wlrcache.Name(kind, name), err)
	}
	return &wlr
}

// wlrExists reports whether the WLR named for (kind, name) in ns survives.
func wlrExists(t *testing.T, r *PolicyReconciler, ns, kind, name string) bool {
	t.Helper()
	var wlr sustainv1alpha1.WorkloadRecommendation
	err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: wlrcache.Name(kind, name)}, &wlr)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get WLR: %v", err)
	}
	return err == nil
}

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
	r := reconcilerForCache(t, governingPolicy("p"), annotatedDeployment("default", "live", "p"), live, orphan, otherPolicy)

	sweep(t, r, "p")

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

// The sweep judges a WorkloadRecommendation by its identity, not by any one
// object's name, or a WLR two members share is deleted out from under them.
func TestSweepWorkloadRecommendations_KeepsOverriddenIdentitySharedByTwoTargets(t *testing.T) {
	member := func(name string) *appsv1.Deployment {
		d := annotatedDeployment("prod", name, "my-policy")
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "app"
		return d
	}
	stored := wlrFor("my-policy", "prod", "Deployment", "app", time.Now().Add(-time.Hour))
	r := reconcilerForCache(t, governingPolicy("my-policy"), member("app-blue"), member("app-green"), stored)

	sweep(t, r, "my-policy")

	var wlr sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: "deployment-app"}
	if err := r.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("expected shared WorkloadRecommendation %v to survive sweep, got: %v", key, err)
	}
}

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

// The strategy-2 periodic sweep: a WLR referencing a vanished policy is deleted,
// but one with an empty spec.policy is left alone.
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

// The reaper collects orphans and nothing else. Under WLR-driven refresh
// "nodata" means "nothing computed YET" and the computation phase retries every
// cycle, so ageing one out would throw away the observed-resources snapshot that
// keeps the identity in the work-list — turning a self-healing state into a cold
// start.
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
			Outcome:    sustainv1alpha1.OutcomeNoData,
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

	r := reconcilerForPolicy(t, policy, mine, other)

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

// TestSweep_RetainsDepartedWorkloadWithinRetention: the workload object is
// gone and the recommendation is within retention — the WLR must survive so
// the dashboard keeps showing the ephemeral workload.
func TestSweep_RetainsDepartedWorkloadWithinRetention(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if !wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR for departed workload deleted within retention window")
	}
}

// conflictedAPI is the cluster of a Conflicted identity, Deployment prod/api,
// whose two members opt into p and q.
func conflictedAPI() []runtime.Object {
	member := func(name, policy string) *appsv1.Deployment {
		d := annotatedDeployment("prod", name, policy)
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "api"
		return d
	}
	return []runtime.Object{governingPolicy("p"), governingPolicy("q"), member("api-blue", "p"), member("api-green", "q")}
}

// A Conflicted identity's frozen Recommendation is kept like a departed one's,
// for the retention window: the members still opting into its Policy keep
// receiving it.
func TestSweep_KeepsConflictedWithinRetention(t *testing.T) {
	r := reconcilerForCache(t, append(conflictedAPI(), wlrFor("p", "prod", "Deployment", "api", time.Now().Add(-time.Hour)))...)
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if !wlrExists(t, r, "prod", "Deployment", "api") {
		t.Error("a Conflicted identity's WLR was deleted within the retention window")
	}
}

// And no longer: nothing recomputes it while its members disagree, and with
// only the webhook's read-time bound to expire it, the object stayed forever.
func TestSweep_DeletesConflictedPastRetention(t *testing.T) {
	r := reconcilerForCache(t, append(conflictedAPI(), wlrFor("p", "prod", "Deployment", "api", time.Now().Add(-80*time.Hour)))...)
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if wlrExists(t, r, "prod", "Deployment", "api") {
		t.Error("a Conflicted identity's WLR survived past the retention window")
	}
}

// TestSweep_DeletesDepartedWorkloadPastRetention: recommendation older than
// the retention window — swept.
func TestSweep_DeletesDepartedWorkloadPastRetention(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-80*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR past retention window survived the sweep")
	}
}

// TestSweep_DeletesOptedOutWorkloadAfterGrace: the Deployment still exists
// but no Policy governs it any more (annotation removed / policy unmatched) —
// retention must NOT apply once past the fresh-write grace.
func TestSweep_DeletesOptedOutWorkloadAfterGrace(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)), dep)
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("WLR for opted-out (still existing) workload must be deleted")
	}
}

// TestSweep_TerminalJobCountsAsGone: a Complete Job is no live member while
// its object lingers until TTL/hook deletion. It did not opt out, so its WLR
// must be retained.
func TestSweep_TerminalJobCountsAsGone(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "argocd-hook"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	r := reconcilerForCache(t, wlrFor("p", "ci", "Job", "argocd-hook", time.Now().Add(-1*time.Hour)), job)
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
	if !wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("WLR for terminal-but-present Job must be retained")
	}
}

// TestSweep_BarePodIdentityAlwaysRetainedUntilExpiry: a bare-pod identity
// between runs has no pod left, so it rides out the retention window.
func TestSweep_BarePodIdentityAlwaysRetainedUntilExpiry(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "airflow", "Pod", "etl", time.Now().Add(-1*time.Hour)))
	r.RecommendationRetention = 72 * time.Hour
	sweep(t, r, "p")
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
	sweep(t, r, "p")
	if wlrExists(t, r, "ci", "Job", "argocd-hook") {
		t.Error("retention=0 must sweep departed WLRs")
	}
}

// A WLR created moments ago (e.g. by the webhook for a pod created after this
// cycle's snapshot) must never be swept — even with retention disabled and
// even when its workload is not governed in the snapshot.
//
// Freshness is expressed as a fresh CreationTimestamp, not a fresh ObservedAt,
// which the computation phase rewrites, so it cannot tell a fresh write from
// this pass's own refresh. See wlrcache.Expired.
func TestSweep_GracePeriodProtectsFreshWrites(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ci", Name: "argocd-hook",
		CreationTimestamp: metav1.Now(),
	}}
	wlr := wlrFor("p", "ci", "Job", "argocd-hook", time.Now())
	wlr.CreationTimestamp = metav1.Now()
	r := reconcilerForCache(t, wlr, job)
	r.RecommendationRetention = 0
	sweep(t, r, "p")
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
	sweep(t, r, "p")
	if !wlrExists(t, r, "airflow", "Pod", "etl") {
		t.Error("status-less freshly created WLR swept mid-write; grace must cover the Create→status-Patch window")
	}
}

// The regression test for the sweep's grace anchor. It must go through a full
// Reconcile: the bug was that the computation phase rewrote status.ObservedAt
// for the now-unmatched identity and the sweep at the end of the SAME pass read
// that as proof of freshness. A direct sweep call with a stale ObservedAt passes
// either way and proves nothing.
//
// Retention is disabled so nothing but the grace period could keep the object.
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

	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "api"), true, policy, dep, wlr)
	r.RecommendationRetention = 0

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wlrExists(t, r, ns, "Deployment", "api") {
		t.Error("WLR for an opted-out but still-running workload survived the sweep; " +
			"the grace period must not be satisfied by this pass's own refresh write")
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
	// Still running, well past the grace period, governed by no Policy: the
	// "opted out" branch, which deletes.
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)), dep)
	r.RecommendationRetention = 72 * time.Hour

	snap := snapshotAll(t, r)
	base := r.Client
	racy := &rewriteOnListClient{Client: base}
	r.Client = racy
	r.sweepWorkloadRecommendations(context.Background(), "p", snap)
	r.Client = base

	if racy.rewrites == 0 {
		t.Fatal("test setup: the sweep never listed the WorkloadRecommendation")
	}
	if !wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("the sweep deleted a WorkloadRecommendation that had been rewritten since it was " +
			"listed; the delete must carry the observed resourceVersion as a precondition")
	}
}

// TestReapOrphans_DoesNotDeleteWLRRewrittenSinceItWasListed: the reaper's
// predicate reads spec.policy, and that field is rewritten in place when a
// re-annotated workload's WLR is re-pointed at its new policy. A copy listed
// just before such a rewrite says "orphan" about an object that has since been
// adopted and recomputed, so the reaper keeps the strict precondition too —
// reaping there would destroy live state exactly as it would in the sweep.
// Unlike the finalizer path it guarantees nothing, so the conflict costs only
// a wait until the next tick.
func TestReapOrphans_DoesNotDeleteWLRRewrittenSinceItWasListed(t *testing.T) {
	// No Policy objects at all, so every WLR reads as an orphan.
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))

	base := r.Client
	racy := &rewriteOnListClient{Client: base}
	r.Client = racy
	err := r.reapOrphanedRecommendations(context.Background())
	r.Client = base

	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if racy.rewrites == 0 {
		t.Fatal("test setup: the reaper never listed the WorkloadRecommendation")
	}
	if !wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("the reaper deleted a WorkloadRecommendation that had been rewritten since it was " +
			"listed; its delete must carry the observed resourceVersion as a precondition")
	}
}

// conflictingWLRDeleteClient fails every WorkloadRecommendation Delete with a
// Conflict — the shape a failed precondition takes on the wire — and leaves
// every other call alone.
type conflictingWLRDeleteClient struct {
	client.Client
	deletes int
}

func (c *conflictingWLRDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	wlr, ok := obj.(*sustainv1alpha1.WorkloadRecommendation)
	if !ok {
		return c.Client.Delete(ctx, obj, opts...)
	}
	c.deletes++
	return apierrors.NewConflict(
		schema.GroupResource{Group: sustainv1alpha1.GroupVersion.Group, Resource: "workloadrecommendations"},
		wlr.Name, errors.New("the object might have been modified"))
}

// TestDeleteAllRecommendationsForPolicy_ConflictIsReturned: on the sweep a
// conflict is benign — nothing depends on that delete having happened, and the
// next cycle re-judges. On the policy-deletion path it is the opposite: the
// caller reads a nil error as "cleanup finished", drops the k8s.sustain.io/cleanup
// finalizer and lets the Policy go, after which this path can never run again.
// The conflict must therefore surface so the reconcile retries.
func TestDeleteAllRecommendationsForPolicy_ConflictIsReturned(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))

	base := r.Client
	racy := &conflictingWLRDeleteClient{Client: base}
	r.Client = racy
	err := r.deleteAllRecommendationsForPolicy(context.Background(), "p")
	r.Client = base

	if racy.deletes == 0 {
		t.Fatal("test setup: the delete path never issued a Delete")
	}
	if err == nil {
		t.Fatal("a conflicting delete must be returned so the finalizer is held and the " +
			"reconcile retries; swallowing it deletes the Policy with its WLRs still present")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want the conflict itself", err)
	}
}

// TestReconcile_PolicyDeletion_ConflictKeepsFinalizer is the same rule seen
// from the caller: a cleanup that could not delete everything must leave the
// finalizer in place, so the Policy stays around for another attempt instead
// of vanishing with its recommendations orphaned until the reaper's next tick.
func TestReconcile_PolicyDeletion_ConflictKeepsFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p",
			Finalizers:        []string{"k8s.sustain.io/cleanup"},
			DeletionTimestamp: &now,
		},
	}
	r := reconcilerForPolicy(t, policy,
		wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))
	r.Client = &conflictingWLRDeleteClient{Client: r.Client}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err == nil {
		t.Fatal("Reconcile returned nil after a failed WLR cleanup; the deletion must be retried")
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if !slices.Contains(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Error("cleanup finalizer removed although recommendations were not deleted")
	}
}

// TestDeleteAllRecommendationsForPolicy_DeletesWLRRewrittenSinceItWasListed is
// the mirror image of TestSweep_DoesNotDeleteWLRRewrittenSinceItWasListed, and
// the two are meant to diverge. The finalizer path's predicate is
// "spec.policy == the policy being deleted", which no rewrite can invalidate,
// so it conditions on the UID alone: a WLR rewritten inside the informer
// cache's propagation window is still deleted rather than being left behind by
// a conflict on a path that promises cleanup.
func TestDeleteAllRecommendationsForPolicy_DeletesWLRRewrittenSinceItWasListed(t *testing.T) {
	r := reconcilerForCache(t, wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour)))

	base := r.Client
	racy := &rewriteOnListClient{Client: base}
	r.Client = racy
	err := r.deleteAllRecommendationsForPolicy(context.Background(), "p")
	r.Client = base

	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if racy.rewrites == 0 {
		t.Fatal("test setup: the delete path never listed the WorkloadRecommendation")
	}
	if wlrExists(t, r, "prod", "Deployment", "web") {
		t.Error("a WorkloadRecommendation rewritten since it was listed survived the policy-deletion " +
			"cleanup; that path must not carry a resourceVersion precondition")
	}
}

// recordingDeleteClient captures the preconditions each cleanup path attaches
// to its deletes, since the fake client enforces only the resourceVersion one.
type recordingDeleteClient struct {
	client.Client
	preconditions []metav1.Preconditions
}

func (c *recordingDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	var o client.DeleteOptions
	o.ApplyOptions(opts)
	if o.Preconditions != nil {
		c.preconditions = append(c.preconditions, *o.Preconditions)
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// TestDeleteWLRsWhere_PreconditionsPerPath pins the difference directly: the
// sweep, whose decision depends on the revision it read, sends UID +
// resourceVersion; the policy-deletion path, whose decision does not, sends the
// UID alone — enough to refuse a name that has been reused by a different
// object, without the rewrite-induced conflict.
func TestDeleteWLRsWhere_PreconditionsPerPath(t *testing.T) {
	const uid = "wlr-uid"
	newWLR := func() *sustainv1alpha1.WorkloadRecommendation {
		wlr := wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour))
		wlr.UID = uid
		return wlr
	}

	for _, tc := range []struct {
		name            string
		run             func(*testing.T, *PolicyReconciler)
		wantResourceVer bool
	}{
		{
			name: "sweep",
			run: func(t *testing.T, r *PolicyReconciler) {
				// Still running, past the grace period, governed by no Policy:
				// the opted-out branch, which deletes.
				r.sweepWorkloadRecommendations(context.Background(), "p", snapshotAll(t, r))
			},
			wantResourceVer: true,
		},
		{
			name: "policy deletion",
			run: func(t *testing.T, r *PolicyReconciler) {
				if err := r.deleteAllRecommendationsForPolicy(context.Background(), "p"); err != nil {
					t.Errorf("delete: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web"}}
			r := reconcilerForCache(t, newWLR(), dep)
			rec := &recordingDeleteClient{Client: r.Client}
			r.Client = rec

			tc.run(t, r)

			if len(rec.preconditions) != 1 {
				t.Fatalf("preconditions recorded = %d, want 1 (one delete)", len(rec.preconditions))
			}
			p := rec.preconditions[0]
			if p.UID == nil || *p.UID != uid {
				t.Errorf("uid precondition = %v, want %q: a reused name must never be deleted blindly", p.UID, uid)
			}
			if gotRV := p.ResourceVersion != nil; gotRV != tc.wantResourceVer {
				t.Errorf("resourceVersion precondition present = %v, want %v", gotRV, tc.wantResourceVer)
			}
		})
	}
}

// The precondition must not break the ordinary case: an untouched WLR is still
// deleted, and a delete of an object someone else already removed still counts
// as done rather than as an error.
func TestDeleteWLRsWhere_DeletesUnchangedAndToleratesAlreadyGone(t *testing.T) {
	present := wlrFor("p", "prod", "Deployment", "web", time.Now().Add(-1*time.Hour))
	r := reconcilerForCache(t, present)

	deleted, listErr, deleteErr := r.deleteWLRsWhere(context.Background(), logr.Discard(), deleteIfUnchanged, nil,
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
