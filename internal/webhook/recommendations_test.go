package webhook

import (
	"context"
	"errors"
	"strings"
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
	// read contract silently.
	if got := wlrName("Deployment", "web"); got != "deployment-web" {
		t.Errorf("wlrName Deployment/web = %q", got)
	}
}

// The truncate-and-hash path for names past the 253-char limit. The expected
// literal is duplicated in the controller package test — both copies of wlrName
// must produce it, or the two disagree on the cache key.
func TestWlrName_LongNameMatchesController(t *testing.T) {
	want := "deployment-" + strings.Repeat("a", 231) + "-55335e7810"
	got := wlrName("Deployment", strings.Repeat("a", 260))
	if got != want {
		t.Errorf("wlrName long input = %q, want %q", got, want)
	}
	if len(got) > 253 {
		t.Errorf("wlrName long input length = %d, want <= 253", len(got))
	}
}

func newRecommendationsTestHandler(t *testing.T, objs ...runtime.Object) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &Handler{Client: c}
}

func cachedQty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func TestFetchRecommendations_FreshHit(t *testing.T) {
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
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected a recommendation, got nil")
	}
	if got["app"].CPURequest == nil || got["app"].CPURequest.MilliValue() != 200 {
		t.Errorf("cpu request mismatch: %v", got["app"].CPURequest)
	}
}

// A WLR past the staleness window returns nil plus the ErrRecommendationStale
// sentinel, which is what lets callers tell it apart from a WLR that does not
// exist yet (e.g. for the recommendation-source metric).
func TestFetchRecommendations_StaleEntryReturnsErrRecommendationStale(t *testing.T) {
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
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if !errors.Is(err, ErrRecommendationStale) {
		t.Fatalf("fetch: expected ErrRecommendationStale, got %v", err)
	}
	if got != nil {
		t.Errorf("stale WLR should return nil recommendation, got %v", got)
	}
}

// MarkNoData stamps ObservedAt once and never refreshes it, so a nodata object
// is older than any staleness window within minutes. It must still report as
// nodata: on a cluster with recurring short Jobs this healthy traffic would
// otherwise dominate the stale signal operators are told to alert on.
func TestFetchRecommendations_NoDataOutlivesStalenessAndStaysNoData(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			// 2h old — far beyond the 30m staleness window, and nothing reaps
			// the object, so this is where it spends most of its life.
			ObservedAt: metav1.NewTime(now.Add(-2 * time.Hour)),
			Source:     sustainv1alpha1.RecommendationSourceNoData,
		},
	}
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if errors.Is(err, ErrRecommendationStale) {
		t.Fatal("a nodata WLR must not report as stale: its mark is stamped once and never " +
			"refreshed, so it would report stale for as long as the identity has no history " +
			"and swamp the stale alert signal")
	}
	if !errors.Is(err, ErrRecommendationNoData) {
		t.Fatalf("fetch: expected ErrRecommendationNoData, got %v", err)
	}
	if got != nil {
		t.Errorf("nodata WLR should return nil recommendation, got %v", got)
	}
}

// A departed identity is still recomputed, but a recompute that finds nothing
// writes nothing, so ObservedAt stays frozen. Gated on staleness, a daily Job
// would run on template resources every time but the first while its retained
// last-known-good sat unused.
func TestFetchRecommendations_DepartedIdentityIsServedDespiteAge(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "job-nightly"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			// A day old: the gap between two runs of a nightly Job, and far
			// beyond any staleness budget.
			ObservedAt: metav1.NewTime(now.Add(-24 * time.Hour)),
			Source:     sustainv1alpha1.RecommendationSourcePrometheus,
			Departed:   true,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: cachedQty("200m")},
			},
		},
	}
	h := newRecommendationsTestHandler(t, wlr)
	got, departed, err := h.fetchRecommendations(context.Background(), "Job", "default", "nightly", now, 30*time.Minute)
	if errors.Is(err, ErrRecommendationStale) {
		t.Fatal("a retained recommendation for a departed identity must not report as stale: " +
			"its ObservedAt is deliberately frozen once its samples age out, so every run after " +
			"the first would start on template resources")
	}
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected the retained recommendation to be served, got nil")
	}
	if got["app"].CPURequest == nil || got["app"].CPURequest.MilliValue() != 200 {
		t.Errorf("cpu request mismatch: %v", got["app"].CPURequest)
	}
	if !departed {
		t.Error("departed must be reported so admit() can count this as retained rather than a fresh hit")
	}
}

// The retained path is bounded by the controller's retention window rather than
// by "whenever the sweep gets round to it": both the sweep and the clearing of
// Departed live inside Reconcile, which returns early whenever collectTargets
// fails, so a wedged controller would otherwise leave the waiver unbounded.
func TestFetchRecommendations_DepartedIdentityIsRejectedPastRetention(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "job-nightly"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			// Older than the 168h default retention: a sweep that ran at all
			// would have deleted this object rather than left it servable.
			ObservedAt: metav1.NewTime(now.Add(-200 * time.Hour)),
			Source:     sustainv1alpha1.RecommendationSourcePrometheus,
			Departed:   true,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: cachedQty("200m")},
			},
		},
	}
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Job", "default", "nightly", now, 30*time.Minute)
	if !errors.Is(err, ErrRecommendationStale) {
		t.Fatalf("a departed recommendation past the retention window must be rejected, got err=%v: "+
			"the waiver relies on a sweep that a wedged controller never runs, so it needs a bound "+
			"of its own", err)
	}
	if got != nil {
		t.Errorf("expected no recommendation past retention, got %v", got)
	}
}

// Departed is what waives the freshness gate, so an UNMARKED object of the same
// age must still trip it — otherwise a broader waiver would pass the test above
// while destroying the stale signal.
func TestFetchRecommendations_UndepartedIdentityStillTripsStaleness(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now.Add(-24 * time.Hour)),
			Source:     sustainv1alpha1.RecommendationSourcePrometheus,
			Departed:   false,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: cachedQty("200m")},
			},
		},
	}
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if !errors.Is(err, ErrRecommendationStale) {
		t.Fatalf("an unmarked stale WLR must still report stale, got %v", err)
	}
	if got != nil {
		t.Errorf("stale WLR should return nil recommendation, got %v", got)
	}
}

// Absence is not an error: "no WLR yet" means admit unmutated, never deny.
func TestFetchRecommendations_MissingReturnsNilNoError(t *testing.T) {
	h := newRecommendationsTestHandler(t)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", time.Now(), 30*time.Minute)
	if err != nil {
		t.Errorf("missing WLR should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("missing WLR should return nil, got %v", got)
	}
}

// The NoLimit intent must round-trip through the read, or the webhook leaves
// the template's limit in place when the policy says to strip it.
func TestFetchRecommendations_PropagatesRemoveFlags(t *testing.T) {
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
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected a recommendation, got nil")
	}
	if !got["app"].RemoveCPULimit {
		t.Error("RemoveCPULimit not propagated")
	}
	if !got["app"].RemoveMemoryLimit {
		t.Error("RemoveMemoryLimit not propagated")
	}
}

// A WLR with no container map is nothing to inject: the controller writes the
// status only once it has at least one container's recommendation.
func TestFetchRecommendations_EmptyContainersReturnsNil(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{},
		},
	}
	h := newRecommendationsTestHandler(t, wlr)
	got, _, err := h.fetchRecommendations(context.Background(), "Deployment", "default", "web", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got != nil {
		t.Errorf("empty container map should return nil, got %v", got)
	}
}
