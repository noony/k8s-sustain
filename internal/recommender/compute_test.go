package recommender

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const mib = float64(mebibyte)

func containers(names ...string) []corev1.Container {
	out := make([]corev1.Container, 0, len(names))
	for _, n := range names {
		out = append(out, corev1.Container{Name: n})
	}
	return out
}

// old dates an identity well past the age gate.
func old() time.Time { return time.Now().Add(-time.Hour) }

func TestCompute_RecommendsEveryObservedContainerWhenNoneDeclared(t *testing.T) {
	res := Compute(Request{
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.5},
			MemPerPod: promclient.ContainerValues{"app": 100 * mib},
			OOM: promclient.OOMSignal{
				OOMCounts:       promclient.ContainerValues{"crashy": 1},
				PeakMemoryBytes: promclient.ContainerValues{"crashy": 300 * mib},
			},
		},
		Since: old(),
	})

	if res.Outcome != Recommended {
		t.Fatalf("outcome = %v, want Recommended", res.Outcome)
	}
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "500m" {
		t.Errorf("app cpu = %v, want 500m", got)
	}
	if got := res.Recs["crashy"].MemoryRequest; got == nil || got.String() != "300Mi" {
		t.Errorf("crashy memory = %v, want 300Mi from the OOM peak with no usage samples", got)
	}
}

// Declared containers bound the result: one Prometheus still reports but the
// workload no longer runs gets nothing, and one with no signal is skipped
// rather than recommended at the hard floor.
func TestCompute_DeclaredContainersBoundTheResult(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app", "nodata"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 1, "renamed-away": 1},
			MemPerPod: promclient.ContainerValues{"app": 64 * mib},
		},
	})

	if len(res.Recs) != 1 || len(res.Containers) != 1 {
		t.Fatalf("recs = %v, details = %v, want app only", res.Recs, res.Containers)
	}
	if rec := res.Recs["app"]; rec.CPURequest == nil || rec.MemoryRequest == nil {
		t.Errorf("app = %+v, want CPU and memory requests", rec)
	}
}

// The age gate holds an identity first seen less than MinWorkloadAge ago, and
// any recent OOM, from Prometheus or the live watcher, excuses it so a
// crash-looping workload is not locked out.
func TestCompute_AgeGate(t *testing.T) {
	now := time.Now()
	long := now.Add(-2 * MinWorkloadAge)
	young := now.Add(-time.Minute)

	cases := []struct {
		name         string
		since        time.Time
		promOOM      bool
		liveOOM      bool
		wantTooYoung bool
	}{
		{"young identity", young, false, false, true},
		{"old identity", long, false, false, false},
		{"young identity, Prometheus OOM bypasses", young, true, false, false},
		{"young identity, live OOM bypasses", young, false, true, false},
		{"undated identity", time.Time{}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 1}}
			if tc.promOOM {
				in.OOM.OOMCounts = promclient.ContainerValues{"side": 1}
			}
			req := Request{Containers: containers("app"), Inputs: in, Since: tc.since}
			if tc.liveOOM {
				req.LiveOOMs = map[string]*oomwatch.OOMRecord{"side": {Container: "side", TerminatedAt: now}}
			}

			res := Compute(req)

			if got := res.Outcome == TooYoung; got != tc.wantTooYoung {
				t.Errorf("TooYoung = %v, want %v (outcome %v)", got, tc.wantTooYoung, res.Outcome)
			}
			// A Simulation still wants the number for a Too young identity.
			if len(res.Recs) != 1 {
				t.Errorf("recs = %v, want app computed regardless of age", res.Recs)
			}
		})
	}
}

// A young workload whose sibling container OOMed gets its recommendation at
// once: the bypass is workload-level, the floor per container.
func TestCompute_YoungWorkloadWithOOMIsRecommended(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app", "side"),
		Inputs: &WorkloadInputs{OOM: promclient.OOMSignal{
			OOMCounts:       promclient.ContainerValues{"app": 1},
			PeakMemoryBytes: promclient.ContainerValues{"app": 80 * mib},
		}},
		Since: time.Now().Add(-time.Minute),
	})

	if res.Outcome != Recommended {
		t.Fatalf("outcome = %v, want Recommended: a recent OOM bypasses the age gate", res.Outcome)
	}
	if rec := res.Recs["app"]; rec.MemoryRequest == nil || rec.MemoryRequest.String() != "80Mi" {
		t.Errorf("app memory = %v, want 80Mi from the OOM peak", rec.MemoryRequest)
	}
	if _, ok := res.Recs["side"]; ok {
		t.Errorf("side has no usage and no OOM, got %+v", res.Recs["side"])
	}
}

func TestCompute_NoDataWhenNothingToRecommendFrom(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app"),
		Inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{}, MemPerPod: promclient.ContainerValues{}},
		Since:      old(),
	})

	if res.Outcome != NoData {
		t.Errorf("outcome = %v, want NoData", res.Outcome)
	}
	if len(res.Recs) != 0 {
		t.Errorf("recs = %v, want none", res.Recs)
	}
}

// A kill the live watcher saw raises the memory floor before Prometheus
// reports it: the OOM-time limit is bumped by DefaultOOMBumpFactor. Only the
// container that was killed is floored.
func TestCompute_LiveOOMRaisesTheMemoryFloor(t *testing.T) {
	killedAt := time.Now().Add(-10 * time.Second)
	res := Compute(Request{
		Containers: containers("app", "side"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.1, "side": 0.05},
			MemPerPod: promclient.ContainerValues{"app": 100 * mib, "side": 100 * mib},
		},
		LiveOOMs: map[string]*oomwatch.OOMRecord{
			"app": {Container: "app", TerminatedAt: killedAt, OOMLimitBytes: 200 << 20},
		},
		Since: old(),
	})

	if got := res.Recs["app"].MemoryRequest; got == nil || got.String() != "240Mi" {
		t.Errorf("app memory = %v, want 240Mi (200Mi limit at the kill x 1.2)", got)
	}
	app := res.Containers["app"]
	if !app.MemFloorApplied {
		t.Error("app: MemFloorApplied = false, want the floor reported")
	}
	if !app.OOM.LiveEventAt.Equal(killedAt) {
		t.Errorf("app: OOM.LiveEventAt = %v, want the kill time %v", app.OOM.LiveEventAt, killedAt)
	}
	if got := res.Recs["side"].MemoryRequest; got == nil || got.String() != "100Mi" {
		t.Errorf("side memory = %v, want its 100Mi percentile: a sibling's kill must not floor it", got)
	}
	if res.Containers["side"].MemFloorApplied {
		t.Error("side: MemFloorApplied = true for a container that was not killed")
	}
}

// Prometheus's OOM-time limit is windowed and can still report the limit from
// before a resize; the live record has the limit applied at the kill. The
// higher one is the limit the container died at.
func TestCompute_OOMAnchorTakesTheHigherOfPrometheusAndLive(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.1},
			MemPerPod: promclient.ContainerValues{"app": 50 * mib},
			OOM: promclient.OOMSignal{
				OOMCounts:     promclient.ContainerValues{"app": 1},
				OOMLimitBytes: promclient.ContainerValues{"app": 96 * mib},
			},
		},
		LiveOOMs: map[string]*oomwatch.OOMRecord{
			"app": {Container: "app", TerminatedAt: time.Now(), OOMLimitBytes: 184 << 20},
		},
		Since: old(),
	})

	if got := res.Recs["app"].MemoryRequest; got == nil || got.Value() <= 184<<20 {
		t.Errorf("app memory = %v, want a bump above the live 184Mi, not the stale 96Mi", got)
	}
}

// The peak rule is not OOM-scoped: every container has a 24h high-water mark.
// Only the container that OOMed is floored at it; an innocent sibling keeps its
// percentile and one with no usage gets nothing.
func TestCompute_SiblingOOMDoesNotFloorInnocentContainer(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app", "side", "nodata"),
		Inputs: &WorkloadInputs{
			MemPerPod: promclient.ContainerValues{"app": 64 * mib, "side": 50 * mib},
			OOM: promclient.OOMSignal{
				OOMCounts:       promclient.ContainerValues{"app": 2},
				PeakMemoryBytes: promclient.ContainerValues{"app": 200 * mib, "side": 180 * mib, "nodata": 150 * mib},
			},
		},
	})

	if got := res.Recs["app"].MemoryRequest; got == nil || got.String() != "200Mi" {
		t.Errorf("app memory = %v, want 200Mi (peak floor)", got)
	}
	if !res.Containers["app"].MemFloorApplied {
		t.Error("app: MemFloorApplied = false")
	}
	if got := res.Recs["side"].MemoryRequest; got == nil || got.String() != "50Mi" {
		t.Errorf("side memory = %v, want its 50Mi percentile", got)
	}
	if res.Containers["side"].MemFloorApplied {
		t.Error("side: the floor must not apply to a container that did not OOM")
	}
	if _, ok := res.Recs["nodata"]; ok {
		t.Errorf("nodata: no OOM and no usage must yield nothing, got %+v", res.Recs["nodata"])
	}
}

// A live kill with no anchor at all (no usage, no peak, no OOM-time limit)
// emits nothing: the only possible value is the 1Mi floor, which guarantees
// the next kill. With the OOM-time limit as its only anchor it bumps above it.
func TestCompute_LiveOOMNeedsAnAnchor(t *testing.T) {
	empty := &WorkloadInputs{CPUPerPod: promclient.ContainerValues{}, MemPerPod: promclient.ContainerValues{}}

	none := Compute(Request{
		Containers: containers("app"),
		Inputs:     empty,
		LiveOOMs:   map[string]*oomwatch.OOMRecord{"app": {Container: "app", TerminatedAt: time.Now()}},
	})
	if _, ok := none.Recs["app"]; ok {
		t.Errorf("live OOM with no anchor must not emit a recommendation, got %v", none.Recs)
	}

	limit := Compute(Request{
		Containers: containers("app"),
		Inputs:     empty,
		LiveOOMs:   map[string]*oomwatch.OOMRecord{"app": {Container: "app", TerminatedAt: time.Now(), OOMLimitBytes: 100 << 20}},
	})
	if got := limit.Recs["app"].MemoryRequest; got == nil || got.String() != "120Mi" {
		t.Errorf("app memory = %v, want 120Mi (100Mi limit x 1.2)", got)
	}
}

func TestCompute_AppliesCoordination(t *testing.T) {
	res := Compute(Request{
		Containers:   containers("app"),
		Inputs:       &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 1}},
		Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true},
		AutoInfo:     autoscaler.Info{Kind: autoscaler.KindHPA, ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 50}},
	})

	// 1 core * 110 / 50 = 2.2 cores; Base keeps the pre-coordination value.
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "2200m" {
		t.Errorf("coordinated cpu = %v, want 2200m", got)
	}
	if got := res.Containers["app"].Base.CPURequest; got == nil || got.String() != "1" {
		t.Errorf("base cpu = %v, want 1", got)
	}
}

// With a ReplicaBudgetAnchor the replica-budget factor follows the overhead.
func TestCompute_AppliesReplicaCorrection(t *testing.T) {
	anchor := 0.0
	res := Compute(Request{
		Containers: containers("app"),
		Inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 0.1}},
		AutoInfo: autoscaler.Info{
			Kind:              autoscaler.KindHPA,
			MinReplicas:       2,
			MaxReplicas:       10,
			CurrentReplicas:   8, // 4x the anchor target, factor clamps to 2.0
			ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 80},
		},
		Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor},
	})

	// Overhead ceil(100x110/80) = 138m, then replica factor 2.0 -> 276m.
	if got := res.Recs["app"].CPURequest.MilliValue(); got != 276 {
		t.Errorf("CPURequest = %dm, want 276m (overhead + replica correction)", got)
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
