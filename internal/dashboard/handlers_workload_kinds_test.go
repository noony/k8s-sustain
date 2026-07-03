package dashboard

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func deploymentWithOwnerName(ns, name, ownerName string, created time.Time) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name}}},
			},
		},
	}
	if ownerName != "" {
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = ownerName
	}
	return d
}

func barePod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

func TestListWorkloadsOfKind_Pod_GroupsByOwnerName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		barePod("airflow", "etl-run-1"),
		barePod("airflow", "etl-run-2"),
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	entries, err := srv.listWorkloadsOfKind(context.Background(), "Pod")
	if err != nil {
		t.Fatalf("listWorkloadsOfKind: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 grouped entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Namespace != "airflow" || e.Name != "etl-daily" {
		t.Errorf("entry = %s/%s, want airflow/etl-daily", e.Namespace, e.Name)
	}
	if e.PolicyAnnotation() != "p" {
		t.Errorf("PolicyAnnotation() = %q, want p", e.PolicyAnnotation())
	}
	if len(e.Containers()) != 1 || e.Containers()[0].Name != "app" {
		t.Errorf("Containers() = %+v, want one container named app", e.Containers())
	}
}

func TestListWorkloadsOfKind_Pod_NoMatchingPods_ReturnsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	entries, err := srv.listWorkloadsOfKind(context.Background(), "Pod")
	if err != nil {
		t.Fatalf("listWorkloadsOfKind: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestGetWorkloadEntry_Pod_FindsGroupByName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		barePod("airflow", "etl-run-1"),
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	e, err := srv.getWorkloadEntry(context.Background(), "airflow", "Pod", "etl-daily")
	if err != nil {
		t.Fatalf("getWorkloadEntry: %v", err)
	}
	if e.Namespace != "airflow" || e.Name != "etl-daily" {
		t.Errorf("entry = %s/%s, want airflow/etl-daily", e.Namespace, e.Name)
	}
}

func TestGetWorkloadEntry_Pod_NoMatch_ReturnsNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	_, err := srv.getWorkloadEntry(context.Background(), "airflow", "Pod", "etl-daily")
	if err == nil {
		t.Fatal("expected an error for a non-existent bare-pod group")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected a NotFound error (so writeK8sGetError maps it to HTTP 404), got: %v", err)
	}
}

// TestListWorkloadsOfKind_Deployment_NoOverride_NameStaysReal is the
// regression case: a Deployment with no owner-name annotation must list and
// address under its own real Kubernetes name, exactly as before
// groupEntriesByIdentity existed.
func TestListWorkloadsOfKind_Deployment_NoOverride_NameStaysReal(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		deploymentWithOwnerName("prod", "web", "", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	entries, err := srv.listWorkloadsOfKind(context.Background(), "Deployment")
	if err != nil {
		t.Fatalf("listWorkloadsOfKind: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "web" {
		t.Fatalf("entries = %+v, want one entry named web", entries)
	}
}

// TestListWorkloadsOfKind_Deployment_Rename_AddressedByOverride verifies a
// single Deployment whose pod template carries owner-name lists under the
// override identity, not its real Kubernetes name — matching what Prometheus
// and the WorkloadRecommendation already key by.
func TestListWorkloadsOfKind_Deployment_Rename_AddressedByOverride(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		deploymentWithOwnerName("prod", "stress", "renamed-app", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	entries, err := srv.listWorkloadsOfKind(context.Background(), "Deployment")
	if err != nil {
		t.Fatalf("listWorkloadsOfKind: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].Name != "renamed-app" {
		t.Errorf("Name = %q, want renamed-app (the real name \"stress\" must not leak through)", entries[0].Name)
	}

	// getWorkloadEntry must find it by the override name...
	e, err := srv.getWorkloadEntry(context.Background(), "prod", "Deployment", "renamed-app")
	if err != nil {
		t.Fatalf("getWorkloadEntry(renamed-app): %v", err)
	}
	if e.Name != "renamed-app" {
		t.Errorf("getWorkloadEntry Name = %q, want renamed-app", e.Name)
	}

	// ...and must NOT find it by the real Kubernetes name, since that's no
	// longer the addressable identity.
	if _, err := srv.getWorkloadEntry(context.Background(), "prod", "Deployment", "stress"); !apierrors.IsNotFound(err) {
		t.Errorf("getWorkloadEntry(stress) = %v, want NotFound (real name is no longer addressable once overridden)", err)
	}
}

// TestListWorkloadsOfKind_Deployment_GroupingCollapsesToOneEntry verifies two
// real Deployments sharing one owner-name override (e.g. blue/green)
// collapse onto a single listed entry, with the most recently created one
// supplying the displayed containers.
func TestListWorkloadsOfKind_Deployment_GroupingCollapsesToOneEntry(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(
		deploymentWithOwnerName("prod", "app-blue", "app", older),
		deploymentWithOwnerName("prod", "app-green", "app", newer),
	).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	entries, err := srv.listWorkloadsOfKind(context.Background(), "Deployment")
	if err != nil {
		t.Fatalf("listWorkloadsOfKind: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 grouped entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Name != "app" {
		t.Errorf("Name = %q, want app", e.Name)
	}
	if len(e.Containers()) != 1 || e.Containers()[0].Name != "app-green" {
		t.Errorf("Containers() = %+v, want the more recently created Deployment's container (app-green)", e.Containers())
	}
}

// TestGetWorkloadEntry_FallsBackToRetainedWLR: the object is gone but a
// retained WLR exists — detail endpoints must still resolve the workload.
func TestGetWorkloadEntry_FallsBackToRetainedWLR(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).
		WithObjects(retainedWLR("p", "airflow", "Pod", "etl")).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}

	e, err := srv.getWorkloadEntry(context.Background(), "airflow", "Pod", "etl")
	if err != nil {
		t.Fatalf("getWorkloadEntry: %v", err)
	}
	if e.PolicyAnnotation() != "p" {
		t.Errorf("PolicyAnnotation = %q, want p", e.PolicyAnnotation())
	}
	cs := e.Containers()
	if len(cs) != 1 || cs[0].Name != "main" {
		t.Fatalf("containers = %+v, want [main]", cs)
	}
	if cpu := cs[0].Resources.Requests.Cpu().String(); cpu != "500m" {
		t.Errorf("cpu request = %s, want 500m", cpu)
	}
}

// TestGetWorkloadEntry_StillNotFoundWithoutWLR: nothing live, nothing
// retained — the NotFound contract is preserved.
func TestGetWorkloadEntry_StillNotFoundWithoutWLR(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}
	_, err := srv.getWorkloadEntry(context.Background(), "airflow", "Pod", "missing")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}
