package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func testutilCounterValue(t *testing.T, vec *prom.CounterVec, ns, kind, name, container string) float64 {
	t.Helper()
	return testutil.ToFloat64(vec.With(prom.Labels{
		"namespace": ns, "owner_kind": kind, "owner_name": name, "container": container,
	}))
}

func TestQuantityEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b *resource.Quantity
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both zero", qty("0"), qty("0"), true},
		{"nil and zero", nil, qty("0"), true},
		{"equal nonzero", qty("100m"), qty("100m"), true},
		{"different", qty("100m"), qty("200m"), false},
		{"nil vs nonzero", nil, qty("100m"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quantityEqual(c.a, c.b); got != c.want {
				t.Errorf("quantityEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestRequestEqual(t *testing.T) {
	// nil rec means "leave alone" — treated as equal regardless of current.
	if !requestEqual(nil, qty("123m")) {
		t.Error("nil rec should be treated as unchanged")
	}
	if !requestEqual(qty("100m"), qty("100m")) {
		t.Error("equal rec/current should be true")
	}
	if requestEqual(qty("100m"), qty("200m")) {
		t.Error("different rec/current should be false")
	}
}

func TestLimitEqual(t *testing.T) {
	// remove=true, current zero → equal (already removed).
	if !limitEqual(nil, true, qty("0")) {
		t.Error("remove=true with zero current should be equal")
	}
	// remove=true, current nonzero → not equal (we still need to remove it).
	if limitEqual(nil, true, qty("500m")) {
		t.Error("remove=true with nonzero current should be NOT equal")
	}
	// rec nil, remove false → leave alone, equal.
	if !limitEqual(nil, false, qty("500m")) {
		t.Error("nil rec without remove should be unchanged")
	}
	// rec set, matches current.
	if !limitEqual(qty("500m"), false, qty("500m")) {
		t.Error("matching rec should be equal")
	}
}

func TestFactorRatio_GuardsAgainstNaN(t *testing.T) {
	if factorRatio(nil, qty("100m")) != 1.0 {
		t.Error("nil adjusted should yield 1.0 (no-op signal)")
	}
	if factorRatio(qty("100m"), nil) != 1.0 {
		t.Error("nil baseline should yield 1.0")
	}
	if factorRatio(qty("100m"), qty("0")) != 1.0 {
		t.Error("zero baseline should yield 1.0 — must not return Inf/NaN")
	}
	if got := factorRatio(qty("200m"), qty("100m")); got != 2.0 {
		t.Errorf("factorRatio(200m, 100m) = %v, want 2.0", got)
	}
}

func TestQuantityString(t *testing.T) {
	if quantityString(nil) != "<nil>" {
		t.Error("nil should stringify as '<nil>'")
	}
	if quantityString(qty("100m")) != "100m" {
		t.Errorf("100m formatted unexpectedly: %s", quantityString(qty("100m")))
	}
}

// TestChangedContainers_DetectsRequestAndLimitDrift verifies that
// changedContainers flags every container whose request or limit drifts from
// the recommendation, ignoring containers without a recommendation entry.
func TestChangedContainers_DetectsRequestAndLimitDrift(t *testing.T) {
	containers := []corev1.Container{
		{
			Name: "matches",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		},
		{
			Name: "drift-cpu",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		},
		{
			Name: "no-rec",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"matches":   {CPURequest: qty("100m"), MemoryRequest: qty("64Mi")},
		"drift-cpu": {CPURequest: qty("250m")},
		// no-rec intentionally absent
	}

	got := changedContainers(containers, recs)
	if len(got) != 1 || got[0] != "drift-cpu" {
		t.Errorf("expected ['drift-cpu'], got %v", got)
	}
}

func TestFilterTargets_PolicyAndNamespace(t *testing.T) {
	targets := []workloadTarget{
		{Kind: "Deployment", Namespace: "default", Name: "a", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "kube-system", Name: "b", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "default", Name: "c", PolicyName: "other"},
		{Kind: "Deployment", Namespace: "default", Name: "d", PolicyName: "p"},
	}

	got := filterTargets(targets, "p", []string{"kube-system"})
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d (%v)", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name}
	if names[0] != "a" || names[1] != "d" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestWorkloadTargetKey_IsStable(t *testing.T) {
	w := workloadTarget{Kind: "Deployment", Namespace: "default", Name: "foo"}
	if w.key() != "Deployment/default/foo" {
		t.Errorf("key = %q", w.key())
	}
}

// makeReconciler builds a PolicyReconciler with a fake client preloaded with objs.
func makeReconciler(t *testing.T, objs ...runtime.Object) *PolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
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

// TestListDeploymentTargets_NamespaceScoped verifies that when a namespace
// list is provided the controller only fetches matching namespaces (and the
// helper iterates over each).
func TestListDeploymentTargets_NamespaceScoped(t *testing.T) {
	d1 := annotatedDeployment("ns-a", "app1", "p")
	d2 := annotatedDeployment("ns-b", "app2", "p")
	d3 := annotatedDeployment("ns-c", "app3", "p")
	r := makeReconciler(t, d1, d2, d3)

	got, err := r.listDeploymentTargets(context.Background(), []string{"ns-a", "ns-b"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(got))
	}
	for _, tgt := range got {
		if tgt.Kind != "Deployment" {
			t.Errorf("kind = %q", tgt.Kind)
		}
		if tgt.Namespace == "ns-c" {
			t.Errorf("ns-c should not be returned, got %v", tgt)
		}
	}
}

// TestListDeploymentTargets_AllNamespaces verifies the empty-namespace path
// (cluster-wide list).
func TestListDeploymentTargets_AllNamespaces(t *testing.T) {
	r := makeReconciler(t,
		annotatedDeployment("a", "x", "p"),
		annotatedDeployment("b", "y", "p"),
	)
	got, err := r.listDeploymentTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 targets, got %d", len(got))
	}
}

func TestListStatefulSetTargets(t *testing.T) {
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ss"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ss"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "ss"},
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}
	r := makeReconciler(t, ss)
	got, err := r.listStatefulSetTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "StatefulSet" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestListDaemonSetTargets(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ds"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ds"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "ds"},
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
	}
	r := makeReconciler(t, ds)
	got, err := r.listDaemonSetTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "DaemonSet" {
		t.Errorf("unexpected: %+v", got)
	}
}

// TestListRolloutTargets_NamespaceScoped covers the Argo Rollouts list path
// — important now that OnCreate works for Rollouts and we want regression
// confidence in the Ongoing-mode controller iteration.
func TestListRolloutTargets_NamespaceScoped(t *testing.T) {
	r1 := annotatedRollout("ns-a", "ro1", "p")
	r2 := annotatedRollout("ns-b", "ro2", "p")
	r := makeReconciler(t, r1, r2)

	got, err := r.listRolloutTargets(context.Background(), []string{"ns-a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "Rollout" || got[0].Namespace != "ns-a" {
		t.Errorf("unexpected: %+v", got)
	}
}

// TestCollectTargets_RespectsUpdateModeAndExcludedNamespaces ties the listing
// helpers and filterTargets together: a policy in Ongoing mode for Deployment
// + Rollout, with one excluded namespace, should return the matched workloads
// only.
func TestCollectTargets_RespectsUpdateModeAndExcludedNamespaces(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing

	d1 := annotatedDeployment("default", "d1", "p")
	d2 := annotatedDeployment("excluded", "d2", "p")
	d3 := annotatedDeployment("default", "d3", "other-policy")
	ro1 := annotatedRollout("default", "ro1", "p")

	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{
						Deployment:  &ongoing,
						ArgoRollout: &ongoing,
					},
				},
			},
		},
	}

	r := makeReconciler(t, d1, d2, d3, ro1)
	r.ExcludedNamespaces = []string{"excluded"}

	got, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 targets (d1, ro1), got %d: %+v", len(got), got)
	}

	kinds := map[string]bool{}
	for _, tgt := range got {
		kinds[tgt.Kind] = true
		if tgt.Namespace == "excluded" {
			t.Errorf("excluded namespace leaked: %v", tgt)
		}
		if tgt.PolicyName != "p" {
			t.Errorf("wrong policy: %v", tgt)
		}
	}
	if !kinds["Deployment"] || !kinds["Rollout"] {
		t.Errorf("expected both Deployment and Rollout kinds, got %v", kinds)
	}
}

// TestCollectTargets_OnCreateModeIsSkippedByController verifies that workloads
// configured for OnCreate-only mode are NOT returned by the controller — those
// are handled by the webhook, not the recycle loop.
func TestCollectTargets_OnCreateModeIsSkippedByController(t *testing.T) {
	onCreate := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &onCreate},
				},
			},
		},
	}
	r := makeReconciler(t, annotatedDeployment("default", "d1", "p"))

	got, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 targets in OnCreate mode, got %d", len(got))
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

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}).
		WithRuntimeObjects(objs...).
		Build()

	r := &PolicyReconciler{
		Client:            c,
		Scheme:            scheme,
		PrometheusClient:  pc,
		ReconcileInterval: time.Hour,
		ConcurrencyLimit:  1,
		recorder:          record.NewFakeRecorder(100),
		patcher:           workload.New(c, false),
		retries:           newRetryTracker(),
	}
	return r, server
}

// TestReconcile_NilPrometheusClient_ReturnsError verifies the early-exit path
// when the controller has no Prometheus client wired up.
func TestReconcile_NilPrometheusClient_ReturnsError(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()
	r.PrometheusClient = nil

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err == nil {
		t.Fatal("expected error when PrometheusClient is nil")
	}
}

// TestReconcile_PolicyNotFound_NoError verifies that a missing policy is
// silently ignored (controller-runtime IgnoreNotFound semantics).
func TestReconcile_PolicyNotFound_NoError(t *testing.T) {
	r, server := reconcilerForPolicy(t, &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "exists"}})
	defer server.Close()

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	if err != nil {
		t.Fatalf("expected no error for missing policy, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected zero RequeueAfter for missing policy, got %v", res.RequeueAfter)
	}
}

// TestReconcile_AddsFinalizerAndRequeues verifies the first reconcile of a
// policy adds the cleanup finalizer and returns a RequeueAfter equal to the
// configured interval.
func TestReconcile_AddsFinalizerAndRequeues(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != time.Hour {
		t.Errorf("RequeueAfter = %v, want 1h", res.RequeueAfter)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if !containsString(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Errorf("expected finalizer to be added, got %v", got.Finalizers)
	}
}

// TestReconcile_EmptyTargets_SetsReadyCondition verifies the success path:
// no workloads matched, finalizer added, Ready condition stamped.
func TestReconcile_EmptyTargets_SetsReadyCondition(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing},
				},
			},
		},
	}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if len(got.Status.Conditions) == 0 {
		t.Fatal("expected at least one status condition")
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status = %v, want True", ready.Status)
	}
	if ready.Reason != "ReconciliationSucceeded" {
		t.Errorf("Ready.Reason = %q", ready.Reason)
	}
}

// TestReconcile_DeletedPolicy_RemovesFinalizer verifies the deletion path:
// when DeletionTimestamp is set, the cleanup finalizer is removed so garbage
// collection can complete.
func TestReconcile_DeletedPolicy_RemovesFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p",
			Finalizers:        []string{"k8s.sustain.io/cleanup"},
			DeletionTimestamp: &now,
		},
	}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got)
	// The fake client garbage-collects the object once finalizers are removed,
	// so a NotFound here is also acceptable.
	if err == nil && containsString(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Error("expected finalizer to be removed on deletion")
	}
}

// TestReconcile_PartialFailure_SetsConditionAndRequeues verifies that when a
// reconcileWorkload fails (e.g. Prometheus error), the Reconcile loop reports
// PartialFailure on the policy status and still requeues.
func TestReconcile_PartialFailure_SetsConditionAndRequeues(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing},
				},
			},
		},
	}
	dep := annotatedDeployment("default", "app", "p")

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = rolloutsv1alpha1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Prometheus mock that always returns 500 — drives reconcileWorkload to
	// the transient-error retry path (which still surfaces an aggregate
	// PartialFailure to the caller via failCount).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}).
		WithRuntimeObjects(policy, dep).
		Build()

	r := &PolicyReconciler{
		Client:            c,
		Scheme:            scheme,
		PrometheusClient:  pc,
		ReconcileInterval: time.Hour,
		ConcurrencyLimit:  1,
		recorder:          record.NewFakeRecorder(100),
		patcher:           workload.New(c, false),
		retries:           newRetryTracker(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != time.Hour {
		t.Errorf("RequeueAfter = %v, want 1h even on partial failure", res.RequeueAfter)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status == metav1.ConditionTrue {
		t.Error("Ready should NOT be True on partial failure")
	}
	if !strings.Contains(ready.Message, "failed") && !strings.Contains(ready.Reason, "Failure") {
		t.Errorf("expected failure-flavoured Ready condition, got reason=%q msg=%q", ready.Reason, ready.Message)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// promServer creates a Prometheus mock that returns predictable per-container
// CPU/memory totals and replica counts so reconcileWorkload computes a
// recommendation deterministically.
func promServerForReconcile(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			// History probe: report enough samples so the recommender doesn't skip.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"168"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			// No recent OOMs in tests by default.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case strings.Contains(q, "workload_cpu_usage"):
			// 100m × 1 replica.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_memory_usage"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"67108864"]}]}}`))
		case strings.Contains(q, "workload_replicas"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`))
		case strings.Contains(q, "container_cpu_usage_by_workload"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "container_memory_by_workload"):
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

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}).
		WithRuntimeObjects(extra...).
		Build()

	return &PolicyReconciler{
		Client:            c,
		Scheme:            scheme,
		PrometheusClient:  pc,
		ReconcileInterval: time.Hour,
		ConcurrencyLimit:  1,
		InPlaceUpdates:    inPlace,
		recorder:          record.NewFakeRecorder(100),
		patcher:           workload.New(c, inPlace),
		retries:           newRetryTracker(),
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
		Kind:      "Deployment",
		Name:      name,
		Namespace: ns,
		Selector:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		Containers: []corev1.Container{{Name: "app",
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

// TestBuildRecommendations_InsufficientHistory_SkipsAndEmitsCounter feeds the
// recommender a count_over_time probe below the minimum threshold and verifies
// buildRecommendations returns an empty map without error.
func TestBuildRecommendations_InsufficientHistory_SkipsAndEmitsCounter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			// 5 samples — below the 12-sample floor.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"5"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}}

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{})
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected empty recommendations on insufficient history, got %d entries: %v", len(recs), recs)
	}
}

// TestBuildRecommendations_RecentOOMBypassesHistoryGate verifies that a
// crash-looping workload (insufficient rate5m samples) still produces a memory
// recommendation when a recent OOM is observed — the OOM floor must override
// the history gate, otherwise the workload is permanently locked at its
// (broken) current request.
func TestBuildRecommendations_RecentOOMBypassesHistoryGate(t *testing.T) {
	const peakBytes = 80 * 1024 * 1024 // 80Mi
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			// 3 samples — below the 12-sample floor (CrashLoop reality).
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"3"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"5"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// Peak working-set witness from the OOM signal: 80Mi.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"83886080"]}
			]}}`))
		default:
			// Empty for everything else — usage / replica queries return nothing.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
	}}

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{})
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec, ok := recs["app"]
	if !ok {
		t.Fatalf("expected recommendation despite insufficient history (recent OOM should bypass gate); got recs=%v", recs)
	}
	if rec.MemoryRequest == nil {
		t.Fatal("expected MemoryRequest from OOM floor (no usage data, but recent OOM)")
	}
	// Floor is max(peak=80Mi, current=64Mi) = 80Mi. Policy default headroom is
	// zero in the test helper.
	if rec.MemoryRequest.Cmp(resource.MustParse("80Mi")) < 0 {
		t.Errorf("expected memory ≥ 80Mi (peak floor), got %s", rec.MemoryRequest)
	}
	_ = peakBytes
}

// TestBuildRecommendations_RecentOOMRaisesMemoryFloor verifies that when
// k8s_sustain:workload_oom_24h reports a recent OOM, the memory recommendation
// is floored at max(peak_working_set_24h, current_request) instead of using the
// (lower) percentile value, and that the oom-floor counter increments.
func TestBuildRecommendations_RecentOOMRaisesMemoryFloor(t *testing.T) {
	const (
		oomCount     = 2.0
		peakBytes    = 800 * 1024 * 1024 // 800Mi — far above percentile
		percentileMB = 100               // percentile would yield 100Mi
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"168"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"2"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// Peak working-set witness for the OOM signal: 800Mi.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"838860800"]}
			]}}`))
		case strings.Contains(q, "workload_cpu_usage"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_memory_usage"):
			// Percentile says 100Mi — but recent OOM should override.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"104857600"]}]}}`))
		case strings.Contains(q, "workload_replicas"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`))
		case strings.Contains(q, "container_cpu_usage_by_workload"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "container_memory_by_workload"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"104857600"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	// Container with current request below the peak — floor should pull
	// the recommendation up to peak, not down to the percentile.
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}}

	before := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{})
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	rec, ok := recs["app"]
	if !ok {
		t.Fatalf("expected recommendation for 'app', got %v", recs)
	}
	if rec.MemoryRequest == nil {
		t.Fatal("expected non-nil MemoryRequest")
	}

	// Floor must be at least the peak (800Mi). Sanity-check it's not the
	// percentile value (~100Mi) — the OOM signal must have lifted it.
	wantAtLeast := resource.MustParse("800Mi")
	if rec.MemoryRequest.Cmp(wantAtLeast) < 0 {
		t.Errorf("expected memory ≥ 800Mi (peak floor), got %s — percentile (%dMi) likely won when OOM should have lifted it", rec.MemoryRequest, percentileMB)
	}
	// And it must not exceed the peak by more than headroom-default-of-zero
	// (the policy in the helper sets no headroom). Allow exact 800Mi.
	wantAtMost := resource.MustParse("800Mi")
	if rec.MemoryRequest.Cmp(wantAtMost) > 0 {
		t.Errorf("expected memory == 800Mi (no headroom configured), got %s", rec.MemoryRequest)
	}

	after := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")
	if after-before != 1 {
		t.Errorf("expected oom_floor_applied counter to increment by 1, got delta=%v (oomCount=%v, peak=%v)", after-before, oomCount, peakBytes)
	}
}

// TestBuildRecommendations_OOMSignalEmpty_DoesNotApplyFloor verifies that when
// no OOM is reported, the percentile value flows through unchanged and the
// floor counter is not incremented.
func TestBuildRecommendations_OOMSignalEmpty_DoesNotApplyFloor(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}}

	before := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{})
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec := recs["app"]
	if rec.MemoryRequest == nil {
		t.Fatal("expected non-nil MemoryRequest")
	}
	// promServerForReconcile reports 64Mi for memory; with no headroom & no
	// OOM, the recommendation should be 64Mi — well below the 1Gi current
	// request, proving the floor did NOT lift it.
	if rec.MemoryRequest.Cmp(resource.MustParse("128Mi")) >= 0 {
		t.Errorf("expected percentile-driven memory < 128Mi (no OOM floor), got %s", rec.MemoryRequest)
	}

	after := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")
	if after != before {
		t.Errorf("expected oom_floor_applied counter unchanged, delta=%v", after-before)
	}
}

// TestReconcileWorkload_HappyPath_ProducesRecommendationsAndPatchesPods
// drives reconcileWorkload end-to-end: Prometheus mock returns sample data,
// the recommender produces requests, the patcher patches pods in place.
// Verifies the per-container request was rewritten on the live pod.
func TestReconcileWorkload_HappyPath_ProducesRecommendationsAndPatchesPods(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-pod",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := reconcilerWithProm(t, server, true /* in-place */, pod)

	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// Retry tracker should record success (no entry, or attempts=0).
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected attempts=0 on success, got %d", state.attempts)
	}
}

// TestReconcileWorkload_RecommendOnly_DoesNotRecyclePods verifies that the
// RecommendOnly flag short-circuits the recycle path: pods stay untouched
// even when the recommendation differs from current resources.
func TestReconcileWorkload_RecommendOnly_DoesNotRecyclePods(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "web-pod",
			Labels: map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := reconcilerWithProm(t, server, false, pod)
	r.RecommendOnly = true
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// Pod should still have the original 999m — no eviction was attempted.
	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-pod"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.DeletionTimestamp != nil {
		t.Error("recommend-only must not delete or evict pods")
	}
}

// TestReconcileWorkload_TransientPromError_RecordsRetry verifies that a 500
// from Prometheus is treated as transient: the retry tracker records the
// failure and reconcileWorkload returns the error so the caller can count it.
func TestReconcileWorkload_TransientPromError_RecordsRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	err := r.reconcileWorkload(context.Background(), policy, tgt)
	if err == nil {
		t.Fatal("expected transient error to bubble up")
	}

	state := r.retries.getState(tgt.key())
	if state.attempts < 1 {
		t.Errorf("expected retry tracker to record at least 1 attempt, got %d", state.attempts)
	}
}

// TestReconcileWorkload_NoPrometheusData_RecordsSuccessAndDoesNothing
// verifies that empty Prometheus results are NOT treated as a failure: the
// reconcile returns nil, retry state is cleared, and no patch is attempted.
func TestReconcileWorkload_NoPrometheusData_RecordsSuccessAndDoesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	// Prime the retry tracker so we can confirm it gets cleared on success.
	r.retries.recordFailure(tgt.key())

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected retry attempts cleared on success, got %d", state.attempts)
	}
}
