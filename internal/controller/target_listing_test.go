package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

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

func TestListCronJobTargets(t *testing.T) {
	cj := annotatedCronJob("default", "nightly", "p")
	r := makeReconciler(t, cj)
	got, err := r.listCronJobTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "CronJob" || got[0].Name != "nightly" {
		t.Errorf("unexpected: %+v", got)
	}
	if got[0].PolicyName != "p" {
		t.Errorf("policy annotation not propagated from JobTemplate: %q", got[0].PolicyName)
	}
	if got[0].Selector != nil {
		t.Errorf("CronJob target should have nil Selector (no pod recycling): %+v", got[0].Selector)
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
