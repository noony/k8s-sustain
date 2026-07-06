package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/workload"
)

func cacheWriteTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "airflow", Name: "etl-run-1"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}}},
	}
}

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// waitForWLR polls the fake client until the WLR's status is populated. The
// write path is asynchronous by design (admission must never wait on it) and
// does a Create followed by a separate Status().Patch, so the object can
// briefly exist with an empty Status — polling on existence alone races with
// that patch and observes a nil ObservedResources.
func waitForWLR(t *testing.T, c client.Client, ns, name string) *sustainv1alpha1.WorkloadRecommendation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var wlr sustainv1alpha1.WorkloadRecommendation
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &wlr); err == nil && len(wlr.Status.ObservedResources) > 0 {
			return &wlr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("WorkloadRecommendation %s/%s status never populated", ns, name)
	return nil
}

// TestWriteRecommendationCache_PersistsBarePodIdentity: a bare-pod identity
// gets its recommendation persisted with the injected resources snapshotted
// as observed — admission is the only moment anything sees these pods.
func TestWriteRecommendationCache_PersistsBarePodIdentity(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}
	recs := map[string]workload.ContainerRecommendation{"main": {CPURequest: qty("250m")}}

	h.writeRecommendationCache(logr.Discard(), "airflow", "Pod", "etl", "p", cacheWriteTestPod(), recs, false)

	wlr := waitForWLR(t, c, "airflow", "pod-etl")
	if wlr.Spec.Policy != "p" || wlr.Spec.WorkloadRef.Kind != "Pod" || wlr.Spec.WorkloadRef.Name != "etl" {
		t.Errorf("spec wrong: %+v", wlr.Spec)
	}
	if got := wlr.Status.Containers["main"].CPURequest; got == nil || got.String() != "250m" {
		t.Errorf("recommendation cpu = %v, want 250m", got)
	}
	// Observed = post-injection value (the pod runs with the recommendation).
	if got := wlr.Status.ObservedResources["main"].CPURequest; got == nil || got.String() != "250m" {
		t.Errorf("observed cpu = %v, want 250m (injected)", got)
	}
}

// TestWriteRecommendationCache_RecommendOnlySnapshotsTemplateValues: with
// recommend-only the pod is not mutated, so observed must be the template
// values, not the recommendation.
func TestWriteRecommendationCache_RecommendOnlySnapshotsTemplateValues(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}
	recs := map[string]workload.ContainerRecommendation{"main": {CPURequest: qty("250m")}}

	h.writeRecommendationCache(logr.Discard(), "airflow", "Pod", "etl", "p", cacheWriteTestPod(), recs, true)

	wlr := waitForWLR(t, c, "airflow", "pod-etl")
	if got := wlr.Status.ObservedResources["main"].CPURequest; got == nil || got.String() != "100m" {
		t.Errorf("observed cpu = %v, want 100m (template, recommend-only)", got)
	}
}

// TestWriteRecommendationCache_SkipsNonEphemeralKinds: Deployments etc. are
// refreshed by the controller loop; the webhook must not double the write
// traffic on busy rollouts. The kind gate runs before the goroutine spawns,
// so asserting immediately is deterministic.
func TestWriteRecommendationCache_SkipsNonEphemeralKinds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}
	recs := map[string]workload.ContainerRecommendation{"main": {CPURequest: qty("250m")}}

	h.writeRecommendationCache(logr.Discard(), "prod", "Deployment", "web", "p", cacheWriteTestPod(), recs, false)

	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: "deployment-web"}, &wlr); err == nil {
		t.Error("Deployment identity must not be written by the webhook")
	}
}
