package controller

import (
	"context"
	"testing"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

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
