package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch/policymatchtest"
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

func TestListDeploymentTargets_NamespaceScoped(t *testing.T) {
	d1 := annotatedDeployment("ns-a", "app1", "p")
	d2 := annotatedDeployment("ns-b", "app2", "p")
	d3 := annotatedDeployment("ns-c", "app3", "p")
	r := makeReconciler(t, d1, d2, d3)

	got, err := r.listTargetsOfKind(context.Background(), "Deployment", []string{"ns-a", "ns-b"})
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

func TestListDeploymentTargets_AllNamespaces(t *testing.T) {
	r := makeReconciler(t,
		annotatedDeployment("a", "x", "p"),
		annotatedDeployment("b", "y", "p"),
	)
	got, err := r.listTargetsOfKind(context.Background(), "Deployment", nil)
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
	got, err := r.listTargetsOfKind(context.Background(), "StatefulSet", nil)
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
	got, err := r.listTargetsOfKind(context.Background(), "CronJob", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "CronJob" || got[0].Name != "nightly" {
		t.Errorf("unexpected: %+v", got)
	}
	// PolicyName is resolved by collectTargets from the three annotation levels;
	// the JobTemplate's pod-template annotations only have to be carried here.
	if got[0].TemplateAnnotations[sustainv1alpha1.PolicyAnnotation] != "p" {
		t.Errorf("policy annotation not propagated from JobTemplate: %q", got[0].TemplateAnnotations[sustainv1alpha1.PolicyAnnotation])
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
	got, err := r.listTargetsOfKind(context.Background(), "DaemonSet", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "DaemonSet" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestListRolloutTargets_NamespaceScoped(t *testing.T) {
	r1 := annotatedRollout("ns-a", "ro1", "p")
	r2 := annotatedRollout("ns-b", "ro2", "p")
	r := makeReconciler(t, r1, r2)

	got, err := r.listTargetsOfKind(context.Background(), "Rollout", []string{"ns-a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "Rollout" || got[0].Namespace != "ns-a" {
		t.Errorf("unexpected: %+v", got)
	}
}

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

// OnCreate-mode workloads ARE collected, so a recommendation is computed and
// cached for them, but they are stamped with UpdateModeOnCreate so
// reconcileWorkload stops before recycling or resizing.
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

	targets, err := r.listBarePodTargets(context.Background(), nil, newNSAnnotations(r.Client))
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

	targets, err := r.listBarePodTargets(context.Background(), nil, newNSAnnotations(r.Client))
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

	targets, err := r.listBarePodTargets(context.Background(), nil, newNSAnnotations(r.Client))
	if err != nil {
		t.Fatalf("listBarePodTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets for an owned pod (handled by listJobTargets instead), got %d", len(targets))
	}
}

// The "bare pods share an owner-name identity but name a different policy" log
// must fire exactly once per reconcile, from the policy that owns the group --
// not from every policy whose selector merely covers the namespace.
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

// Replays the shared contract table (policymatchtest.AnnotationCases) against
// the controller's own wiring, so the three annotation levels resolve here
// exactly as the shared resolver says.
func TestCollectTargets_AnnotationLevels(t *testing.T) {
	for _, tc := range policymatchtest.AnnotationCases() {
		t.Run(tc.Name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "team-a", Annotations: tc.Namespace,
			}}
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: "web", Annotations: tc.Workload,
			}}
			d.Spec.Template.Annotations = tc.Template
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}

			r := makeReconciler(t, ns, d)
			policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
			policy.Spec.RightSizing.Update.Types.Deployment = ptrUpdateMode(sustainv1alpha1.UpdateModeOngoing)

			got, err := r.collectTargets(context.Background(), policy)
			if err != nil {
				t.Fatalf("collectTargets: %v", err)
			}
			wantCount := 0
			if tc.WantPolicy == "p" {
				wantCount = 1
			}
			if len(got) != wantCount {
				t.Fatalf("expected %d targets for case %q (resolver says policy=%q level=%q), got %d",
					wantCount, tc.Name, tc.WantPolicy, tc.WantLevel, len(got))
			}
		})
	}
}

// A Namespace naming a Policy whose selector does not reach it gets nothing: the
// Namespace chooses among the policies offered to it, it cannot grant itself one.
//
// Selector.Namespaces is deliberately left EMPTY (cluster-wide) rather than
// scoped away from team-a: scoping it there would make listKindTargets skip the
// namespace before the Deployment is ever listed, and the test would pass with
// filterTargets never evaluating anything. The rejection is routed through
// filterTargets' policymatch.Matches call via a LabelSelector instead.
func TestCollectTargets_NamespaceOptInStillHonoursSelector(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a",
		Name:      "web",
		Labels:    map[string]string{"team": "a"},
	}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}

	r := makeReconciler(t, ns, d)
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	// No Selector.Namespaces: cluster-wide, so team-a IS listed. The selector
	// excludes the Deployment on labels instead, routing the rejection
	// through filterTargets' policymatch.Matches call.
	policy.Spec.Selector.LabelSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"team": "b"},
	}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrUpdateMode(sustainv1alpha1.UpdateModeOngoing)

	got, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a namespace must not opt into a policy whose selector excludes it, got %+v", got)
	}
}

// The annotation walk must be lazy: zero Namespace reads when the pod template
// already decides. Fetching the Namespace unconditionally would couple every
// pod-template-only workload to a read it never needed, so a failure there
// would abort a reconcile for workloads untouched by the Namespace level.
func TestCollectTargets_PodTemplateOptIn_NoNamespaceReads(t *testing.T) {
	d := annotatedDeployment("team-a", "web", "p")

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	var nsGets int
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					nsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &PolicyReconciler{Client: c, Scheme: scheme}

	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrUpdateMode(sustainv1alpha1.UpdateModeOngoing)

	got, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("expected the pod-template-annotated deployment, got %+v", got)
	}
	if nsGets != 0 {
		t.Errorf("expected 0 Namespace Gets for a pod-template-only opt-in, got %d — "+
			"the walk must not fetch a level it does not need", nsGets)
	}
}

// A non-NotFound Namespace Get failure must propagate through collectTargets
// (namespace_annotations_test.go covers the same failure in isolation). The
// Deployment carries no pod-template or workload-level annotation, so the lazy
// walk falls through to the Namespace — which is what makes the Get reachable.
func TestCollectTargets_NamespaceReadError_Propagates(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme core: %v", err)
	}

	boom := apierrors.NewInternalError(errors.New("boom"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &PolicyReconciler{Client: c, Scheme: scheme}

	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = ptrUpdateMode(sustainv1alpha1.UpdateModeOngoing)

	_, err := r.collectTargets(context.Background(), policy)
	if err == nil {
		t.Fatal("expected a non-NotFound namespace read failure to propagate out of collectTargets")
	}
}

func ptrUpdateMode(m sustainv1alpha1.UpdateMode) *sustainv1alpha1.UpdateMode { return &m }

// TestDedupeNamespaces covers the collapse of a selector namespace list the
// apiserver happily accepts with repeats (the field is a plain atomic array
// with no uniqueness constraint). Order of first appearance must survive, and
// nil/empty must stay nil/empty because that is what means "all namespaces".
func TestDedupeNamespaces(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil stays nil", in: nil, want: nil},
		{name: "empty stays empty", in: []string{}, want: []string{}},
		{name: "single", in: []string{"prod"}, want: []string{"prod"}},
		{name: "no duplicates keeps order", in: []string{"b", "a", "c"}, want: []string{"b", "a", "c"}},
		{name: "adjacent duplicates", in: []string{"prod", "prod"}, want: []string{"prod"}},
		{name: "non-adjacent duplicates keep first position", in: []string{"b", "a", "b", "c", "a"}, want: []string{"b", "a", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeNamespaces(tc.in)
			if tc.in == nil && got != nil {
				t.Fatalf("nil input must stay nil, got %v", got)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("dedupeNamespaces(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupeNamespaces(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// A namespace named twice in spec.selector.namespaces must yield one target per
// workload: two copies are dispatched to two errgroup goroutines that then race
// over the per-workload retry state and the WorkloadRecommendation write.
func TestListKindTargets_DuplicateNamespacesListedOnce(t *testing.T) {
	d1 := annotatedDeployment("ns-a", "app1", "p")
	d2 := annotatedDeployment("ns-b", "app2", "p")
	r := makeReconciler(t, d1, d2)

	got, err := r.listTargetsOfKind(context.Background(), "Deployment", []string{"ns-a", "ns-b", "ns-a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, tgt := range got {
			names = append(names, tgt.key())
		}
		t.Fatalf("expected 2 targets from a duplicated namespace list, got %d (%v)", len(got), names)
	}
}

func TestCollectTargets_DuplicateSelectorNamespaces(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				Namespaces: []string{"prod", "prod"},
			},
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing},
				},
			},
		},
	}
	r := makeReconciler(t, annotatedDeployment("prod", "web", "p"))

	got, err := r.collectTargets(context.Background(), policy)
	if err != nil {
		t.Fatalf("collectTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 target, got %d (%+v)", len(got), got)
	}
}
