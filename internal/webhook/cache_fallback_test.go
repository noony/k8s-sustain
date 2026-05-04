package webhook

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestWlrName_MatchesController(t *testing.T) {
	// Webhook and controller must agree on the object name. Drift breaks the
	// fallback contract silently.
	if got := wlrName("Deployment", "web"); got != "deployment-web" {
		t.Errorf("wlrName Deployment/web = %q", got)
	}
}

func newFallbackHandler(t *testing.T, objs ...runtime.Object) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &Handler{Client: c}
}

func cachedQty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

// TestFetchCachedRecommendations_FreshHit verifies a recent WLR is returned.
func TestFetchCachedRecommendations_FreshHit(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			Policy:      "p",
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "default", Name: "web"},
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now.Add(-5 * time.Minute)),
			Source:     "prometheus",
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: cachedQty("200m"), MemoryRequest: cachedQty("256Mi")},
			},
		},
	}
	h := newFallbackHandler(t, wlr)
	got, err := h.fetchCachedRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected cached recommendation, got nil")
	}
	if got["app"].CPURequest == nil || got["app"].CPURequest.MilliValue() != 200 {
		t.Errorf("cpu request mismatch: %v", got["app"].CPURequest)
	}
}

// TestFetchCachedRecommendations_StaleEntryReturnsNil verifies the freshness
// gate: a WLR older than the staleness window is treated as missing, so the
// webhook won't inject very-old data.
func TestFetchCachedRecommendations_StaleEntryReturnsNil(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			// 2h old — beyond the 30m staleness window.
			ObservedAt: metav1.NewTime(now.Add(-2 * time.Hour)),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: cachedQty("200m")},
			},
		},
	}
	h := newFallbackHandler(t, wlr)
	got, err := h.fetchCachedRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != nil {
		t.Errorf("stale WLR should return nil, got %v", got)
	}
}

// TestFetchCachedRecommendations_MissingReturnsNilNoError verifies absence is
// not an error — the webhook treats "no fallback" as "fail open".
func TestFetchCachedRecommendations_MissingReturnsNilNoError(t *testing.T) {
	h := newFallbackHandler(t)
	got, err := h.fetchCachedRecommendations(context.Background(), "Deployment", "default", "web", time.Now(), 30*time.Minute)
	if err != nil {
		t.Errorf("missing WLR should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("missing WLR should return nil, got %v", got)
	}
}

// TestFetchCachedRecommendations_PropagatesRemoveFlags verifies that the
// NoLimit intent persisted on the WorkloadRecommendation status round-trips
// through the cache fallback. Without this, a Prometheus outage would cause
// the webhook to leave the template's existing limit in place even when the
// policy says to strip it.
func TestFetchCachedRecommendations_PropagatesRemoveFlags(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {
					CPURequest:        cachedQty("200m"),
					MemoryRequest:     cachedQty("256Mi"),
					RemoveCPULimit:    true,
					RemoveMemoryLimit: true,
				},
			},
		},
	}
	h := newFallbackHandler(t, wlr)
	got, err := h.fetchCachedRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected cached recommendation, got nil")
	}
	if !got["app"].RemoveCPULimit {
		t.Error("RemoveCPULimit not propagated through cache fallback")
	}
	if !got["app"].RemoveMemoryLimit {
		t.Error("RemoveMemoryLimit not propagated through cache fallback")
	}
}

// TestFetchCachedRecommendations_EmptyContainersReturnsNil verifies a WLR
// with no container map is treated as no fallback (controller writes the
// status only when it has at least one container's recommendation).
func TestFetchCachedRecommendations_EmptyContainersReturnsNil(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{},
		},
	}
	h := newFallbackHandler(t, wlr)
	got, err := h.fetchCachedRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != nil {
		t.Errorf("empty container map should return nil, got %v", got)
	}
}
