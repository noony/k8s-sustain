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

// TestWlrName_LongNameMatchesController verifies the truncate-and-hash path
// for names exceeding the 253-char object-name limit. The expected literal is
// duplicated in the controller package test — both copies of wlrName must
// produce it, or controller and webhook disagree on the cache key.
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

// TestFetchRecommendations_FreshHit verifies a recent WLR is returned.
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

// TestFetchRecommendations_StaleEntryReturnsErrRecommendationStale verifies
// the freshness gate: a WLR older than the staleness window returns a nil
// recommendation (the webhook won't inject very-old data) AND the
// ErrRecommendationStale sentinel, distinguishing it from a WLR that simply
// doesn't exist yet -- callers use errors.Is to tell the two apart (e.g. for
// the recommendation-source metric).
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

// A nodata mark is stamped once by wlrcache.MarkNoData and then never touched
// again (it returns early on every later cycle), so within minutes the object
// is older than any staleness window and stays that way for as long as the
// identity has no history — with no reap to bound it. It must still report as
// nodata, not as stale: operators are told to alert on a rising stale ratio
// ("the controller has fallen behind, is stuck, or Prometheus is down"), and on
// a cluster with recurring short Jobs or bare-pod groups this healthy traffic
// would otherwise dominate that signal while the nodata bucket stayed empty.
func TestFetchRecommendations_NoDataOutlivesStalenessAndStaysNoData(t *testing.T) {
	now := time.Now()
	wlr := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deployment-web"},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			// 2h old — far beyond the 30m staleness window. Nothing collects
			// the object, so this is the state it spends most of its life in
			// until the identity finally accumulates history.
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

// A departed identity IS recomputed every cycle, but once its samples age out
// of the query window that recompute finds nothing and deliberately writes
// nothing, so ObservedAt stays frozen at the last successful write. Gated on
// staleness, a daily Job would then be admitted on template resources on every
// run but its first — for the whole retention window — while the retained
// last-known-good recommendation sat unused. The
// controller marks these Departed precisely so the read can tell them apart
// from a recommendation it is merely failing to refresh.
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

// The retained path is bounded, and the bound is the controller's retention
// window rather than "whenever the sweep gets round to it".
//
// Both the clearing of Departed (discovery's EnsureExists) and the sweep that
// deletes a lapsed object live inside Reconcile, which returns early before
// either whenever collectTargets fails — RBAC revoked on one workload kind, an
// unreachable API group, a removed CRD. A wedged controller therefore freezes
// the flag set, and an unbounded waiver would have the webhook injecting that
// identity's last-known-good on every admission forever, at any age, including
// after the workload came back to life. That is precisely the "the controller
// has fallen behind" case the staleness gate exists for, and it was the one
// case where it was silently disabled.
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

// The other half of the pin: Departed is what waives the freshness gate, so an
// UNMARKED object of the same age must still trip it. Without this, a change
// that waived staleness more broadly would leave the test above passing while
// silently destroying the stale signal — the alert operators are told to watch
// for a stuck controller or a Prometheus outage.
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

// TestFetchRecommendations_MissingReturnsNilNoError verifies absence is not
// an error — the webhook treats "no WLR yet" as "admit unmutated", never as a
// denial.
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

// TestFetchRecommendations_PropagatesRemoveFlags verifies that the NoLimit
// intent persisted on the WorkloadRecommendation status round-trips through
// the read. Without this, the webhook would leave the template's existing
// limit in place even when the policy says to strip it.
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

// TestFetchRecommendations_EmptyContainersReturnsNil verifies a WLR with no
// container map is treated as nothing to inject (the controller writes the
// status only when it has at least one container's recommendation).
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
