package workload

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func annotationsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestOwnerAnnotations(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(annotationsScheme(t)).WithObjects(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "web", Annotations: map[string]string{"k": "v"},
		}},
	).Build()

	got, err := OwnerAnnotations(context.Background(), c, "ns", "Deployment", "web")
	if err != nil || got["k"] != "v" {
		t.Fatalf("existing owner: got %v, %v", got, err)
	}
	got, err = OwnerAnnotations(context.Background(), c, "ns", "Deployment", "gone")
	if err != nil || got != nil {
		t.Fatalf("missing owner: got %v, %v; want nil, nil", got, err)
	}
	got, err = OwnerAnnotations(context.Background(), c, "ns", "CustomKind", "x")
	if err != nil || got != nil {
		t.Fatalf("unknown kind: got %v, %v; want nil, nil", got, err)
	}
}

func TestOwnerAnnotations_ReturnsNonNotFoundErrors(t *testing.T) {
	boom := errors.New("boom")
	c := fake.NewClientBuilder().WithScheme(annotationsScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	if _, err := OwnerAnnotations(context.Background(), c, "ns", "Deployment", "web"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
}

func TestNamespaceAnnotations(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(annotationsScheme(t)).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", Annotations: map[string]string{"k": "v"}}},
	).Build()

	got, err := NamespaceAnnotations(context.Background(), c, "team")
	if err != nil || got["k"] != "v" {
		t.Fatalf("existing namespace: got %v, %v", got, err)
	}
	got, err = NamespaceAnnotations(context.Background(), c, "gone")
	if err != nil || got != nil {
		t.Fatalf("missing namespace: got %v, %v; want nil, nil", got, err)
	}
}
