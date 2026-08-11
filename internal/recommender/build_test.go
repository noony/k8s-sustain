package recommender

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

func container(name string) corev1.Container { return corev1.Container{Name: name} }

// TestBuildContainerRecs_CollectsHasDataAndSkipsRest verifies the shared loop
// emits a recommendation for every container with CPU/memory signal and skips
// containers with no data (the HasData gate shared by controller and webhook).
func TestBuildContainerRecs_CollectsHasDataAndSkipsRest(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1},
		MemPerPod: promclient.ContainerValues{"app": 64 * float64(mebibyte)},
		OOM:       promclient.OOMSignal{PeakMemoryBytes: promclient.ContainerValues{}, OOMLimitBytes: promclient.ContainerValues{}},
	}
	containers := []corev1.Container{container("app"), container("nodata")}

	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{},
	)

	if len(recs) != 1 {
		t.Fatalf("expected 1 rec (app), got %d: %v", len(recs), recs)
	}
	rec, ok := recs["app"]
	if !ok {
		t.Fatalf("expected rec for app, got %v", recs)
	}
	if rec.CPURequest == nil || rec.MemoryRequest == nil {
		t.Fatalf("expected cpu+mem request for app, got %+v", rec)
	}
	if _, ok := recs["nodata"]; ok {
		t.Fatalf("container with no signal should be skipped")
	}
}

// TestBuildContainerRecs_OnResultPerHasDataContainer verifies OnResult fires
// once per container that has data and not for skipped containers — this is the
// controller's metric-emission hook contract.
func TestBuildContainerRecs_OnResultPerHasDataContainer(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1, "side": 0.2},
		MemPerPod: promclient.ContainerValues{},
		OOM:       promclient.OOMSignal{PeakMemoryBytes: promclient.ContainerValues{}, OOMLimitBytes: promclient.ContainerValues{}},
	}
	containers := []corev1.Container{container("app"), container("nodata"), container("side")}

	var seen []string
	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{
			OnResult: func(name string, res ContainerRecResult) {
				if !res.HasData {
					t.Errorf("OnResult called with HasData=false for %s", name)
				}
				seen = append(seen, name)
			},
		},
	)

	if len(recs) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(recs))
	}
	if len(seen) != 2 {
		t.Fatalf("expected OnResult to fire for the 2 has-data containers, got %v", seen)
	}
	for _, n := range seen {
		if n == "nodata" {
			t.Fatalf("OnResult must not fire for skipped container")
		}
	}
}

// TestBuildContainerRecs_EnrichOOMDrivesMemoryFloor verifies EnrichOOM can fold
// a live OOM observation into the signal before compute — the controller's
// live-OOM merge. Without the live event the container has no memory signal and
// gets no memory request; with it, the OOM peak drives a floored memory rec.
func TestBuildContainerRecs_EnrichOOMDrivesMemoryFloor(t *testing.T) {
	peak := 200 * float64(mebibyte)
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{},
		MemPerPod: promclient.ContainerValues{},
		OOM: promclient.OOMSignal{
			PeakMemoryBytes: promclient.ContainerValues{"app": peak},
			OOMLimitBytes:   promclient.ContainerValues{},
		},
	}
	containers := []corev1.Container{container("app")}

	// Baseline: no OOM counts, no enrichment -> no memory emission (no usage,
	// no recent OOM, no live event).
	base := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{},
	)
	if _, ok := base["app"]; ok {
		t.Fatalf("without OOM signal app should have no recommendation, got %v", base)
	}

	// With EnrichOOM stamping a live event, the OOM peak (present in inputs)
	// drives a floored memory recommendation.
	var floorApplied bool
	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{
			EnrichOOM: func(name string, sig OOMSignal) OOMSignal {
				sig.LiveEventAt = time.Now()
				return sig
			},
			OnResult: func(name string, res ContainerRecResult) {
				floorApplied = res.MemFloorApplied
			},
		},
	)
	rec, ok := recs["app"]
	if !ok || rec.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation from live-OOM peak, got %v", recs)
	}
	if !floorApplied {
		t.Fatalf("expected MemFloorApplied=true from OOM peak floor")
	}
	// 200Mi peak with no headroom rounds to 200Mi.
	if got := rec.MemoryRequest.String(); got != "200Mi" {
		t.Fatalf("expected 200Mi memory request from peak floor, got %s", got)
	}
}

// TestBuildContainerRecs_SiblingOOMDoesNotFloorInnocentContainer pins the
// per-container OOM-recency scoping: when container "app" OOMed but its
// sibling "side" did not, only app gets the peak floor. The sibling keeps its
// pure percentile recommendation even though the (non-OOM-scoped) peak rule
// reports a 24h high-water mark for it, and a third sibling with no usage
// data gets NO memory recommendation via the OOM bypass.
func TestBuildContainerRecs_SiblingOOMDoesNotFloorInnocentContainer(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{},
		MemPerPod: promclient.ContainerValues{
			"app":  64 * float64(mebibyte),
			"side": 50 * float64(mebibyte),
		},
		OOM: promclient.OOMSignal{
			// Only app OOMed.
			OOMCounts: promclient.ContainerValues{"app": 2},
			// The peak rule is not OOM-scoped: every container has a 24h
			// high-water mark, including the innocent siblings.
			PeakMemoryBytes: promclient.ContainerValues{
				"app":    200 * float64(mebibyte),
				"side":   180 * float64(mebibyte),
				"nodata": 150 * float64(mebibyte),
			},
			OOMLimitBytes: promclient.ContainerValues{},
		},
	}
	containers := []corev1.Container{container("app"), container("side"), container("nodata")}

	floorApplied := map[string]bool{}
	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{
			OnResult: func(name string, res ContainerRecResult) {
				floorApplied[name] = res.MemFloorApplied
			},
		},
	)

	app, ok := recs["app"]
	if !ok || app.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation for OOMed container, got %v", recs)
	}
	if got := app.MemoryRequest.String(); got != "200Mi" {
		t.Errorf("app: expected 200Mi (peak floor), got %s", got)
	}
	if !floorApplied["app"] {
		t.Errorf("app: expected MemFloorApplied=true")
	}

	side, ok := recs["side"]
	if !ok || side.MemoryRequest == nil {
		t.Fatalf("expected percentile memory recommendation for sibling, got %v", recs)
	}
	if got := side.MemoryRequest.String(); got != "50Mi" {
		t.Errorf("side: expected 50Mi (pure percentile, no sibling-OOM floor), got %s", got)
	}
	if floorApplied["side"] {
		t.Errorf("side: floor must not apply to a container that did not OOM")
	}

	if _, ok := recs["nodata"]; ok {
		t.Errorf("nodata: a container that did not OOM and has no usage must get no recommendation, got %v", recs["nodata"])
	}
}

// TestWorkloadInputs_HasRecentOOM verifies the workload-level recency helper
// (the age-gate bypass) fires when ANY container OOMed.
func TestWorkloadInputs_HasRecentOOM(t *testing.T) {
	none := &WorkloadInputs{}
	if none.HasRecentOOM() {
		t.Error("empty signal must not report recent OOM")
	}
	one := &WorkloadInputs{OOM: promclient.OOMSignal{OOMCounts: promclient.ContainerValues{"side": 1}}}
	if !one.HasRecentOOM() {
		t.Error("an OOM in any container must report recent OOM at workload level")
	}
}

// TestBuildContainerRecs_LiveOOMWithoutAnchorEmitsNothing verifies a live OOM
// event with no usable memory anchor (no usage samples, no peak, no OOM-time
// limit) emits NO memory recommendation. Emitting would floor at the hard 1Mi
// minimum and guarantee the next OOM for a crash-looping container.
func TestBuildContainerRecs_LiveOOMWithoutAnchorEmitsNothing(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{},
		MemPerPod: promclient.ContainerValues{},
		OOM: promclient.OOMSignal{
			PeakMemoryBytes: promclient.ContainerValues{},
			OOMLimitBytes:   promclient.ContainerValues{},
		},
	}
	containers := []corev1.Container{container("app")}

	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{
			EnrichOOM: func(name string, sig OOMSignal) OOMSignal {
				sig.LiveEventAt = time.Now()
				return sig
			},
		},
	)
	if _, ok := recs["app"]; ok {
		t.Fatalf("live OOM with zero anchor must not emit a recommendation, got %v", recs)
	}
}

// TestBuildContainerRecs_LiveOOMWithLimitAnchorEmits verifies a live OOM event
// whose only anchor is the OOM-time cgroup limit (e.g. captured by the live
// watcher before Prometheus surfaces it) still drives a bumped memory
// recommendation: 100Mi limit × 1.20 bump = 120Mi.
func TestBuildContainerRecs_LiveOOMWithLimitAnchorEmits(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{},
		MemPerPod: promclient.ContainerValues{},
		OOM: promclient.OOMSignal{
			PeakMemoryBytes: promclient.ContainerValues{},
			OOMLimitBytes:   promclient.ContainerValues{},
		},
	}
	containers := []corev1.Container{container("app")}

	recs := BuildContainerRecs(
		containers, inputs,
		autoscaler.Info{Kind: autoscaler.KindNone},
		sustainv1alpha1.ResourcesConfigs{},
		sustainv1alpha1.AutoscalerCoordination{},
		BuildContainerRecsOptions{
			EnrichOOM: func(name string, sig OOMSignal) OOMSignal {
				sig.LiveEventAt = time.Now()
				sig.OOMTimeLimitBytes = 100 * float64(mebibyte)
				return sig
			},
		},
	)
	rec, ok := recs["app"]
	if !ok || rec.MemoryRequest == nil {
		t.Fatalf("expected memory recommendation from live-OOM limit anchor, got %v", recs)
	}
	if got := rec.MemoryRequest.String(); got != "120Mi" {
		t.Fatalf("expected 120Mi (100Mi × 1.20 bump), got %s", got)
	}
}

// TestShouldSkipYoungWorkload covers the age gate, including the
// identity-first-seen input: an identity k8s-sustain has known for a while
// (e.g. prior runs of a re-created standalone Job) makes the workload count as
// old even when the object itself was just created. A known identity can only
// make a workload look older, never younger.
func TestShouldSkipYoungWorkload(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * MinWorkloadAge)
	young := now.Add(-time.Minute)

	cases := []struct {
		name              string
		created           time.Time
		identityFirstSeen time.Time
		recentOOM         bool
		want              bool
	}{
		{"young object, unknown identity", young, time.Time{}, false, true},
		{"young object, long-known identity", young, old, false, false},
		{"young object, newly-known identity", young, now.Add(-2 * time.Minute), false, true},
		{"old object, unknown identity", old, time.Time{}, false, false},
		{"old object, newly-known identity stays old", old, young, false, false},
		{"young object, recent OOM bypasses", young, time.Time{}, true, false},
		// Bare pods: the object age is unknown (zero), so how long the
		// identity has been known is the only signal — a newly-known identity
		// gates, a long-known one or no signal at all does not.
		{"zero created, newly-known identity", time.Time{}, young, false, true},
		{"zero created, long-known identity", time.Time{}, old, false, false},
		{"zero created, unknown identity", time.Time{}, time.Time{}, false, false},
		{"zero created, newly-known identity, recent OOM bypasses", time.Time{}, young, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldSkipYoungWorkload(tc.created, tc.identityFirstSeen, tc.recentOOM); got != tc.want {
				t.Errorf("ShouldSkipYoungWorkload(%v, %v, %v) = %v, want %v",
					tc.created, tc.identityFirstSeen, tc.recentOOM, got, tc.want)
			}
		})
	}
}

func TestAgeForLog(t *testing.T) {
	if got := AgeForLog(time.Time{}); got != "none" {
		t.Errorf("AgeForLog(zero) = %q, want %q", got, "none")
	}
	got := AgeForLog(time.Now().Add(-30 * time.Minute))
	if !strings.HasPrefix(got, "30m") {
		t.Errorf("AgeForLog(30m ago) = %q, want ~30m duration string", got)
	}
}

// TestBuildContainerRecs_AppliesReplicaCorrection verifies that
// BuildContainerRecs applies the full replica-budget correction end to end
// (overhead, then the replica-budget factor) when the Policy configures a
// ReplicaBudgetAnchor — this is now the only path: BuildContainerRecsOptions
// no longer has a way to suppress it (see the removed DisableReplicaCorrection
// field; it existed only for admission-time computation, which no longer
// happens — the webhook now serves a value the controller already computed
// on its reconcile cadence, where replica counts reflect steady state).
func TestBuildContainerRecs_AppliesReplicaCorrection(t *testing.T) {
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1}, // 100m per pod
		OOM:       promclient.OOMSignal{PeakMemoryBytes: promclient.ContainerValues{}, OOMLimitBytes: promclient.ContainerValues{}},
	}
	containers := []corev1.Container{container("app")}
	anchor := 0.0 // target_replicas = minReplicas = 2
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		MinReplicas:       2,
		MaxReplicas:       10,
		CurrentReplicas:   8, // 4× the anchor target, factor clamps to 2.0
		ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 80},
	}
	coord := sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor}

	// Overhead ceil(100×110/80) = 138m, then replica factor 2.0 → 276m.
	recs := BuildContainerRecs(containers, inputs, info,
		sustainv1alpha1.ResourcesConfigs{}, coord, BuildContainerRecsOptions{})
	if got := recs["app"].CPURequest.MilliValue(); got != 276 {
		t.Errorf("CPURequest = %dm, want 276m (overhead + replica correction)", got)
	}
}
