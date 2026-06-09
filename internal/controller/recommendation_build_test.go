package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
)

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

// TestBuildRecommendations_YoungWorkload_SkipsAndEmitsCounter verifies that a
// workload created less than minWorkloadAge ago is skipped — the CPU rate
// hasn't stabilized yet, so the percentile would floor to ~0 and trigger an
// immediate recycle on the next reconcile.
func TestBuildRecommendations_YoungWorkload_SkipsAndEmitsCounter(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}}

	// 1 minute old — well under the 10-minute gate.
	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected empty recommendations for young workload, got %d entries: %v", len(recs), recs)
	}
}

// TestBuildRecommendations_SparseSignal_StillProducesRecommendation verifies
// that a workload with only a few samples in the policy window (e.g. a daily
// CronJob with a 2d window) still gets a recommendation as long as it's old
// enough — the percentile queries handle sparseness, the gate must not.
func TestBuildRecommendations_SparseSignal_StillProducesRecommendation(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}}

	// 2 hours old — clears the age gate; the mock returns sparse but non-empty
	// CPU/memory totals, mimicking a cronjob that just finished a run.
	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("expected non-empty recommendations for old workload with usage data")
	}
	if rec := recs["app"]; rec.CPURequest == nil {
		t.Error("expected CPU recommendation, got nil")
	}
}

// TestBuildRecommendations_RecentOOMBypassesHistoryGate verifies that a
// crash-looping workload (insufficient CPU rate samples) still produces a memory
// recommendation when a recent OOM is observed — the OOM floor must override
// the history gate, otherwise the workload is permanently locked at its
// (broken) current request.
func TestBuildRecommendations_RecentOOMBypassesHistoryGate(t *testing.T) {
	const peakBytes = 80 * 1024 * 1024 // 80Mi
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			// 3 samples — below the 12-sample floor (CrashLoop reality).
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"3"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"5"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// Peak working-set witness from the OOM signal: 80Mi.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"83886080"]}
			]}}`))
		default:
			// Empty for everything else — usage / replica queries return nothing.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
	}}

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec, ok := recs["app"]
	if !ok {
		t.Fatalf("expected recommendation despite insufficient history (recent OOM should bypass gate); got recs=%v", recs)
	}
	if rec.MemoryRequest == nil {
		t.Fatal("expected MemoryRequest from OOM floor (no usage data, but recent OOM)")
	}
	// Floor is max(peak=80Mi, current=64Mi) = 80Mi. Policy default headroom is
	// zero in the test helper.
	if rec.MemoryRequest.Cmp(resource.MustParse("80Mi")) < 0 {
		t.Errorf("expected memory ≥ 80Mi (peak floor), got %s", rec.MemoryRequest)
	}
	_ = peakBytes
}

// TestBuildRecommendations_RecentOOMRaisesMemoryFloor verifies that when
// k8s_sustain:workload_oom_24h reports a recent OOM, the memory recommendation
// is floored at max(peak_working_set_24h, current_request) instead of using the
// (lower) percentile value, and that the oom-floor counter increments.
func TestBuildRecommendations_RecentOOMRaisesMemoryFloor(t *testing.T) {
	const (
		oomCount     = 2.0
		peakBytes    = 800 * 1024 * 1024 // 800Mi — far above percentile
		percentileMB = 100               // percentile would yield 100Mi
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"168"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"2"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// Peak working-set witness for the OOM signal: 800Mi.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"838860800"]}
			]}}`))
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			// Percentile says 100Mi — but recent OOM should override.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"104857600"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	// Container with current request below the peak — floor should pull
	// the recommendation up to peak, not down to the percentile.
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}}

	before := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	rec, ok := recs["app"]
	if !ok {
		t.Fatalf("expected recommendation for 'app', got %v", recs)
	}
	if rec.MemoryRequest == nil {
		t.Fatal("expected non-nil MemoryRequest")
	}

	// Floor must be at least the peak (800Mi). Sanity-check it's not the
	// percentile value (~100Mi) — the OOM signal must have lifted it.
	wantAtLeast := resource.MustParse("800Mi")
	if rec.MemoryRequest.Cmp(wantAtLeast) < 0 {
		t.Errorf("expected memory ≥ 800Mi (peak floor), got %s — percentile (%dMi) likely won when OOM should have lifted it", rec.MemoryRequest, percentileMB)
	}
	// And it must not exceed the peak by more than headroom-default-of-zero
	// (the policy in the helper sets no headroom). Allow exact 800Mi.
	wantAtMost := resource.MustParse("800Mi")
	if rec.MemoryRequest.Cmp(wantAtMost) > 0 {
		t.Errorf("expected memory == 800Mi (no headroom configured), got %s", rec.MemoryRequest)
	}

	after := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")
	if after-before != 1 {
		t.Errorf("expected oom_floor_applied counter to increment by 1, got delta=%v (oomCount=%v, peak=%v)", after-before, oomCount, peakBytes)
	}
}

// TestBuildRecommendations_OOMTimeLimitBumpsBeyondLimit verifies that when
// peak working-set is unreliable (cgroup v2 / sub-scrape OOM kill) but the
// OOM-time memory limit signal is present, the recommendation is bumped above
// the limit the kernel killed at. Without this floor the recommendation would
// drop to the percentile and the workload would OOM forever.
func TestBuildRecommendations_OOMTimeLimitBumpsBeyondLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "count_over_time"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"168"]}]}}`))
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"9"]}]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// Peak underreports — cgroup v2 sub-scrape spike missed.
			// 36Mi, well below the 96Mi limit the kernel killed at.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"37748736"]}
			]}}`))
		case strings.Contains(q, "container_oom_limit_24h:bytes"):
			// 96Mi — the cgroup limit at the moment of OOM.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"100663296"]}
			]}}`))
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"0.01"]}]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			// Percentile says 40Mi — the steady-state baseline; OOM floor should override.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"container":"app"},"value":[0,"41943040"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("96Mi")},
		},
	}}

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec, ok := recs["app"]
	if !ok || rec.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation, got recs=%v", recs)
	}
	// Floor = limit_at_oom * bump_factor = 96Mi * 1.20 = 115.2Mi → rounded up to 116Mi.
	// Bare minimum: must be above the 96Mi limit the kernel killed at.
	if rec.MemoryRequest.Cmp(resource.MustParse("96Mi")) <= 0 {
		t.Errorf("expected memory > 96Mi (bump above OOM-time limit), got %s — recommendation would re-OOM", rec.MemoryRequest)
	}
}

// TestBuildRecommendations_SiblingOOMDoesNotFloorInnocentContainer pins the
// per-container OOM scoping end to end: container "app" OOMed but its sidecar
// "side" did not. The OOM recording rule keeps the container label, so only
// app gets the peak floor; the sidecar keeps its pure percentile value even
// though the (non-OOM-scoped) peak rule reports a 24h high-water mark for it.
func TestBuildRecommendations_SiblingOOMDoesNotFloorInnocentContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_oom_24h"):
			// Only app OOMed.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"2"]}
			]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			// The peak rule is NOT OOM-scoped: both containers have a 24h
			// high-water mark well above their percentile.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"838860800"]},
				{"metric":{"container":"side"},"value":[0,"629145600"]}
			]}}`))
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"0.1"]},
				{"metric":{"container":"side"},"value":[0,"0.05"]}
			]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			// Percentile says 100Mi for both.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"104857600"]},
				{"metric":{"container":"side"},"value":[0,"104857600"]}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}, {Name: "side"}}

	sideFloorBefore := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "side")

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	app, ok := recs["app"]
	if !ok || app.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation for OOMed container, got %v", recs)
	}
	if app.MemoryRequest.Cmp(resource.MustParse("800Mi")) != 0 {
		t.Errorf("app: expected 800Mi (peak floor), got %s", app.MemoryRequest)
	}

	side, ok := recs["side"]
	if !ok || side.MemoryRequest == nil {
		t.Fatalf("expected percentile memory recommendation for sidecar, got %v", recs)
	}
	if side.MemoryRequest.Cmp(resource.MustParse("100Mi")) != 0 {
		t.Errorf("side: expected 100Mi (pure percentile, sibling OOM must not floor it), got %s", side.MemoryRequest)
	}

	sideFloorAfter := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "side")
	if sideFloorAfter != sideFloorBefore {
		t.Errorf("side: oom_floor_applied counter must not increment for an innocent sibling, delta=%v", sideFloorAfter-sideFloorBefore)
	}
}

// TestBuildRecommendations_YoungWorkload_SiblingOOMBypassesAgeGate verifies
// the age-gate bypass stays workload-level: any container's OOM (here, "app")
// unblocks the too-young skip so the crash-looping container can get a
// recommendation immediately.
func TestBuildRecommendations_YoungWorkload_SiblingOOMBypassesAgeGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_oom_24h"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"1"]}
			]}}`))
		case strings.Contains(q, "container_peak_memory_24h:bytes"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"83886080"]}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}, {Name: "side"}}

	// 1 minute old — under the 10-minute gate, but app's OOM bypasses it.
	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec, ok := recs["app"]
	if !ok || rec.MemoryRequest == nil {
		t.Fatalf("expected OOM-anchored recommendation despite young workload, got %v", recs)
	}
	if _, ok := recs["side"]; ok {
		t.Errorf("side: no usage and no OOM must yield no recommendation, got %v", recs["side"])
	}
}

// fakeOOMSource is a canned oomwatch.Source for live-OOM tests.
type fakeOOMSource struct {
	records map[string]*oomwatch.OOMRecord
}

func (f *fakeOOMSource) Recent(_, _, _, container string, _ time.Duration) *oomwatch.OOMRecord {
	return f.records[container]
}

func (f *fakeOOMSource) RecentByWorkload(_, _, _ string, _ time.Duration) map[string]*oomwatch.OOMRecord {
	return f.records
}

// TestBuildRecommendations_LiveOOMFloorsOnlyOOMedContainer verifies the live
// OOM watcher merge stays per-container: a live OOM record for "app" floors
// app's memory (bump above the captured cgroup limit) while the sidecar keeps
// its percentile recommendation.
func TestBuildRecommendations_LiveOOMFloorsOnlyOOMedContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"0.1"]},
				{"metric":{"container":"side"},"value":[0,"0.05"]}
			]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			// Percentile says 100Mi for both.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"104857600"]},
				{"metric":{"container":"side"},"value":[0,"104857600"]}
			]}}`))
		default:
			// No Prometheus OOM signal yet — the live watcher is ahead of it.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	r.LiveOOM = LiveOOMConfig{
		Source: &fakeOOMSource{records: map[string]*oomwatch.OOMRecord{
			"app": {
				Container:     "app",
				TerminatedAt:  time.Now().Add(-10 * time.Second),
				OOMLimitBytes: 209715200, // 200Mi cgroup limit at kill time
			},
		}},
		TriggerCh: make(chan event.GenericEvent),
	}
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{Name: "app"}, {Name: "side"}}

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}

	app, ok := recs["app"]
	if !ok || app.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation for live-OOMed container, got %v", recs)
	}
	// Floor = 200Mi × 1.20 bump = 240Mi.
	if app.MemoryRequest.Cmp(resource.MustParse("200Mi")) <= 0 {
		t.Errorf("app: expected bump above the 200Mi OOM-time limit, got %s", app.MemoryRequest)
	}

	side, ok := recs["side"]
	if !ok || side.MemoryRequest == nil {
		t.Fatalf("expected percentile memory recommendation for sidecar, got %v", recs)
	}
	if side.MemoryRequest.Cmp(resource.MustParse("100Mi")) != 0 {
		t.Errorf("side: expected 100Mi (live OOM on sibling must not floor it), got %s", side.MemoryRequest)
	}
}

// TestBuildRecommendations_OOMSignalEmpty_DoesNotApplyFloor verifies that when
// no OOM is reported, the percentile value flows through unchanged and the
// floor counter is not incremented.
func TestBuildRecommendations_OOMSignalEmpty_DoesNotApplyFloor(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */)
	policy := policyForReconcileWorkload(t, "p")
	containers := []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}}

	before := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")

	recs, err := r.buildRecommendations(context.Background(), policy, "default", "Deployment", "web", containers, autoscaler.Info{}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("buildRecommendations: %v", err)
	}
	rec := recs["app"]
	if rec.MemoryRequest == nil {
		t.Fatal("expected non-nil MemoryRequest")
	}
	// promServerForReconcile reports 64Mi for memory; with no headroom & no
	// OOM, the recommendation should be 64Mi — well below the 1Gi current
	// request, proving the floor did NOT lift it.
	if rec.MemoryRequest.Cmp(resource.MustParse("128Mi")) >= 0 {
		t.Errorf("expected percentile-driven memory < 128Mi (no OOM floor), got %s", rec.MemoryRequest)
	}

	after := testutilCounterValue(t, oomFloorApplied, "default", "Deployment", "web", "app")
	if after != before {
		t.Errorf("expected oom_floor_applied counter unchanged, delta=%v", after-before)
	}
}
