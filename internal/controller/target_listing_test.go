package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestFilterTargets_PolicyAndNamespace(t *testing.T) {
	targets := []workloadTarget{
		{Kind: "Deployment", Namespace: "default", Name: "a", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "kube-system", Name: "b", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "default", Name: "c", PolicyName: "other"},
		{Kind: "Deployment", Namespace: "default", Name: "d", PolicyName: "p"},
	}

	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	got := filterTargets(targets, policy, []string{"kube-system"})
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d (%v)", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name}
	if names[0] != "a" || names[1] != "d" {
		t.Errorf("unexpected names: %v", names)
	}
}

// TestFilterTargets_SelectorNamespaces verifies that
// policy.Spec.Selector.Namespaces narrows the target list to only the listed
// namespaces; this is the controller-side enforcement that mirrors the
// webhook's new check.
func TestFilterTargets_SelectorNamespaces(t *testing.T) {
	targets := []workloadTarget{
		{Kind: "Deployment", Namespace: "production", Name: "a", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "staging", Name: "b", PolicyName: "p"},
		{Kind: "Deployment", Namespace: "default", Name: "c", PolicyName: "p"},
	}
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				Namespaces: []string{"production"},
			},
		},
	}
	got := filterTargets(targets, policy, nil)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected only target a (production), got %+v", got)
	}
}

// TestFilterTargets_LabelSelector verifies that
// policy.Spec.Selector.LabelSelector is enforced — targets whose workload
// labels do not satisfy the selector are dropped.
func TestFilterTargets_LabelSelector(t *testing.T) {
	targets := []workloadTarget{
		{Kind: "Deployment", Namespace: "default", Name: "matching", PolicyName: "p", Labels: map[string]string{"team": "platform"}},
		{Kind: "Deployment", Namespace: "default", Name: "other-team", PolicyName: "p", Labels: map[string]string{"team": "growth"}},
		{Kind: "Deployment", Namespace: "default", Name: "no-labels", PolicyName: "p"},
	}
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "platform"},
				},
			},
		},
	}
	got := filterTargets(targets, policy, nil)
	if len(got) != 1 || got[0].Name != "matching" {
		t.Fatalf("expected only matching target, got %+v", got)
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

// TestCollectTargets_OnCreateModeIsCollectedWithMode verifies that workloads
// configured for OnCreate-only mode ARE now returned by the controller (so
// reconcileWorkload can compute+cache a recommendation for them) but are
// stamped with UpdateModeOnCreate so reconcileWorkload's mode gate can stop
// before recycling/resizing — the webhook remains the only mutation path for
// OnCreate.
func TestCollectTargets_OnCreateModeIsCollectedWithMode(t *testing.T) {
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
	if len(got) != 1 {
		t.Fatalf("expected 1 target in OnCreate mode (collected for recommendation caching), got %d", len(got))
	}
	if got[0].UpdateMode != sustainv1alpha1.UpdateModeOnCreate {
		t.Errorf("UpdateMode = %q, want OnCreate", got[0].UpdateMode)
	}
}

// TestCollectTargets_IncludesOnCreateKindsWithMode verifies OnCreate-mode
// kinds are collected (so they get recommendations + WLR cache writes) and
// stamped with their mode.
func TestCollectTargets_IncludesOnCreateKindsWithMode(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "hook"}}
	job.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}

	r := makeReconciler(t, job)

	onCreate := sustainv1alpha1.UpdateModeOnCreate
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
	}
	policy.Spec.RightSizing.Update.Types.Job = &onCreate

	targets, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 (OnCreate Job must be collected)", len(targets))
	}
	if targets[0].UpdateMode != sustainv1alpha1.UpdateModeOnCreate {
		t.Errorf("UpdateMode = %q, want OnCreate", targets[0].UpdateMode)
	}
}

func TestListBarePodTargets_GroupsByNamespaceAndOwnerName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	podA1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "old-container"}}},
	}
	podA2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-2", CreationTimestamp: metav1.NewTime(newer),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "new-container"}}},
	}
	// Same owner-name, different namespace — must NOT collapse with podA1/podA2.
	podB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow-staging", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(podA1, podA2, podB).Build()
	r := &PolicyReconciler{Client: fc}

	targets, err := r.listBarePodTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("listBarePodTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (one per namespace), got %d: %+v", len(targets), targets)
	}
	for _, target := range targets {
		if target.Kind != "Pod" || target.IdentityKind != "Pod" {
			t.Errorf("Kind/IdentityKind = %s/%s, want Pod/Pod", target.Kind, target.IdentityKind)
		}
		if target.Name != "etl-daily" || target.IdentityName != "etl-daily" {
			t.Errorf("Name/IdentityName = %s/%s, want etl-daily/etl-daily", target.Name, target.IdentityName)
		}
		if target.Namespace == "airflow" {
			if len(target.Containers) != 1 || target.Containers[0].Name != "new-container" {
				t.Errorf("expected the more recently created pod's container, got %+v", target.Containers)
			}
		}
	}
}

func TestListBarePodTargets_NoOwnerNameAnnotation_NotDiscovered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "standalone",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &PolicyReconciler{Client: fc}

	targets, err := r.listBarePodTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("listBarePodTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets for a pod with no owner-name annotation, got %d", len(targets))
	}
}

func TestListBarePodTargets_OwnedPod_NotDiscovered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	ctrlBool := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "owned",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "etl-daily-job", Controller: &ctrlBool}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &PolicyReconciler{Client: fc}

	targets, err := r.listBarePodTargets(context.Background(), nil)
	if err != nil {
		t.Fatalf("listBarePodTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets for an owned pod (handled by listJobTargets instead), got %d", len(targets))
	}
}

// TestCollectTargets_BarePodPolicyMismatchLoggedOnlyByOwningPolicy pins the
// F4 follow-up fix: the "bare pods share an owner-name identity but name a
// different policy" log must fire exactly once per reconcile, from the
// policy that actually owns the group -- not from every policy whose
// selector merely happens to cover the namespace. Before the fix,
// listBarePodTargets logged this itself, BEFORE filterTargets narrowed the
// result down to the group's own policy, so a policy uninvolved on either
// side of the conflict would still report it, every reconcile, forever.
func TestCollectTargets_BarePodPolicyMismatchLoggedOnlyByOwningPolicy(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	podOwner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(newer),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	podMismatched := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-2", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "other-policy",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	r := makeReconciler(t, podOwner, podMismatched)

	ongoing := sustainv1alpha1.UpdateModeOngoing
	owningPolicy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	owningPolicy.Spec.RightSizing.Update.Types.Pod = &ongoing

	// uninvolvedPolicy shares the namespace (an empty selector.namespaces
	// covers everything, same as owningPolicy's) and also has Pod enabled, so
	// its collectTargets call lists and groups the exact same pods -- but it
	// names neither side of the conflict.
	uninvolvedPolicy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "uninvolved"}}
	uninvolvedPolicy.Spec.RightSizing.Update.Types.Pod = &ongoing

	const marker = "bare pods share an owner-name identity but name a different policy"

	capture := func(policy *sustainv1alpha1.Policy) string {
		var lines []string
		logger := funcr.New(func(prefix, args string) {
			lines = append(lines, prefix+" "+args)
		}, funcr.Options{})
		ctx := log.IntoContext(context.Background(), logger)
		if _, err := r.collectTargets(ctx, policy); err != nil {
			t.Fatalf("collectTargets: %v", err)
		}
		return strings.Join(lines, "\n")
	}

	if out := capture(owningPolicy); !strings.Contains(out, marker) {
		t.Errorf("owning policy %q must log the mismatch; got:\n%s", owningPolicy.Name, out)
	}
	if out := capture(uninvolvedPolicy); strings.Contains(out, marker) {
		t.Errorf("uninvolved policy %q must NOT report a conflict naming someone else's policies; got:\n%s",
			uninvolvedPolicy.Name, out)
	}
}
