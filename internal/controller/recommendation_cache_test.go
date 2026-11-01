package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// governingPolicy manages every kind, so whatever opts into it is governed.
func governingPolicy(name string) *sustainv1alpha1.Policy {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	return policyManaging(name, sustainv1alpha1.UpdateTypes{
		Deployment: &ongoing, StatefulSet: &ongoing, DaemonSet: &ongoing, ArgoRollout: &ongoing,
		CronJob: &ongoing, Job: &ongoing, Pod: &ongoing,
	})
}

// wlrFor builds a WorkloadRecommendation labeled for policyName whose target
// is (kind, ns, name) and whose recommendation was last observed at observedAt.
func wlrFor(policyName, ns, kind, name string, observedAt time.Time) *sustainv1alpha1.WorkloadRecommendation {
	cpu := resource.MustParse("100m")
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      wlrcache.Name(kind, name),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: policyName},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: ns, Name: name},
			Policy:      policyName,
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(observedAt),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"main": {CPURequest: &cpu}},
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

// fetchHook runs a hook when the pass fetches its inputs: after the pass took
// its snapshot, before it sweeps.
type fetchHook struct {
	recommender.InputsFetcher
	hook func()
}

func (f fetchHook) FetchInputs(ctx context.Context, at time.Time, cfg sustainv1alpha1.ResourcesConfigs, reqs []recommender.InputsRequest) map[promclient.WorkloadIdentity]recommender.InputsResult {
	f.hook()
	return f.InputsFetcher.FetchInputs(ctx, at, cfg, reqs)
}

// The pass sweeps the Policy's objects by where their identities stand in the
// snapshot it took, as of when it took it. Its clock moves twelve minutes on
// during the fetch, as for a pass that waits long: a webhook stub created a
// minute after the snapshot is in no snapshot, and survives only because the
// grace is counted from the snapshot. The pass records on the same clock.
func TestReconcile_SweepsAgainstItsSnapshotAtItsInstant(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakeClock(now)
	p := governingPolicy("p")
	p.Spec.Selector.Namespaces = []string{"prod"}
	member := func(name, policy, ownerName string) *appsv1.Deployment {
		d := annotatedDeployment("prod", name, policy)
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = ownerName
		return d
	}
	optout := annotatedDeployment("prod", "optout", "p")
	delete(optout.Spec.Template.Annotations, sustainv1alpha1.PolicyAnnotation)
	c := fakeClientBuilder(testFullScheme(t)).WithObjects(
		p, governingPolicy("q"), optout,
		member("app-blue", "p", "app"), member("app-green", "p", "app"),
		member("api-blue", "p", "api"), member("api-green", "q", "api"),
		wlrFor("p", "prod", "Deployment", "optout", now.Add(-time.Hour)),
		wlrFor("p", "prod", "Deployment", "app", now.Add(-time.Hour)),
		wlrFor("p", "prod", "Deployment", "api", now.Add(-80*time.Hour)),
		wlrFor("p", "prod", "Job", "hook", now.Add(-time.Hour)),
		wlrFor("p", "other", "Deployment", "web", now.Add(-time.Hour)),
	).Build()
	stub := wlrFor("p", "prod", "Pod", "late", time.Time{})
	stub.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
	stub.CreationTimestamp = metav1.NewTime(now.Add(time.Minute))
	inputs := fetchHook{InputsFetcher: usageFor("prod", "Deployment", "app"), hook: func() {
		clk.Step(12 * time.Minute)
		if err := c.Create(context.Background(), stub.DeepCopy()); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Errorf("creating the stub: %v", err)
		}
	}}
	r := reconcilerOn(c, c.Scheme(), inputs, false)
	r.Clock = clk
	r.RecommendationRetention = 72 * time.Hour

	reconcileOnce(t, r, "p")

	if got := getWLRFor(t, r, "prod", "Deployment", "app").Status.ObservedAt; !got.Time.Equal(clk.Now()) {
		t.Errorf("app observedAt = %v, want the reconciler's clock at the record, %v", got.Time, clk.Now())
	}

	for _, tc := range []struct {
		ns, kind, name string
		kept           bool
		why            string
	}{
		{"prod", "Deployment", "optout", false, "its members opted out"},
		{"prod", "Deployment", "app", true, "the Policy governs its owner-name group"},
		{"prod", "Deployment", "api", false, "it is Conflicted past the retention window"},
		{"prod", "Job", "hook", true, "it is Departed within the retention window"},
		{"other", "Deployment", "web", false, "its namespace left the Policy's scope"},
		{"prod", "Pod", "late", true, "it was created after the snapshot"},
	} {
		if got := wlrExists(t, r, tc.ns, tc.kind, tc.name); got != tc.kept {
			t.Errorf("%s %s/%s kept = %v, want %v: %s", tc.kind, tc.ns, tc.name, got, tc.kept, tc.why)
		}
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

// A cleanup that could not delete everything must leave the finalizer in
// place, so the Policy stays around for another attempt instead of vanishing
// with its recommendations orphaned until the reaper's next tick.
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
	racy := &conflictingWLRDeleteClient{Client: r.Client}
	r.Client = racy

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err == nil {
		t.Fatal("Reconcile returned nil after a failed WLR cleanup; the deletion must be retried")
	}
	if racy.deletes == 0 {
		t.Fatal("test setup: the cleanup never issued a Delete")
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if !slices.Contains(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Error("cleanup finalizer removed although recommendations were not deleted")
	}
}
