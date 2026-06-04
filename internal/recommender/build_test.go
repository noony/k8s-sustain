package recommender

import (
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
		containers, inputs, false,
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
		containers, inputs, false,
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

	// Baseline: recentOOM=false, no enrichment -> no memory emission (no usage,
	// no recent OOM, no live event).
	base := BuildContainerRecs(
		containers, inputs, false,
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
		containers, inputs, false,
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
