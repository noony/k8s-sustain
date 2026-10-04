package recommender

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

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
			OOM:       map[string]OOM{"crashy": {Kills: 1, PeakBytes: 300 * mib, HasPeak: true}},
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
				in.OOM = map[string]OOM{"side": {Kills: 1}}
			}
			req := Request{Containers: containers("app"), Inputs: in, Since: tc.since}
			if tc.liveOOM {
				req.LiveOOMs = map[string]*oomwatch.OOMRecord{"side": liveKill("side", 0, 0)}
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

// computeApp computes the one container "app" from in under rs.
func computeApp(in *WorkloadInputs, rs sustainv1alpha1.ResourcesConfigs) ContainerRecResult {
	return Compute(Request{Containers: containers("app"), Inputs: in, Resources: rs}).Containers["app"]
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

// The CPU trace records the value after every stage that ran: the percentile
// at full precision, headroom rounded up, a minAllowed clamp moving it, both
// coordination factors, and the limit derived from the final request.
func TestCompute_TracesCPUStages(t *testing.T) {
	anchor := 0.0
	res := Compute(Request{
		Containers: containers("app"),
		Inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 0.1234}},
		Resources: sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{
			Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](10), MinAllowed: qtyp("150m")},
			Limits:   sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true},
		}},
		Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor},
		AutoInfo: autoscaler.Info{
			Kind:              autoscaler.KindHPA,
			MinReplicas:       2,
			MaxReplicas:       10,
			CurrentReplicas:   8, // 4x the anchor target, factor clamps to 2.0
			ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 80},
		},
		Since: old(),
	})

	cpu := res.Containers["app"].Trace.CPU
	if cpu == nil {
		t.Fatal("no CPU trace")
	}
	assertQty(t, "percentile", cpu.Percentile, "123400u")
	assertQty(t, "withHeadroom", &cpu.WithHeadroom, "136m") // ceil(123.4m x 1.1)
	assertQty(t, "clamped", &cpu.Clamped, "150m")           // raised to minAllowed
	c := cpu.Coordination
	if c == nil {
		t.Fatal("no coordination stage")
	}
	if c.OverheadFactor != 1.375 || c.ReplicaFactor == nil || *c.ReplicaFactor != 2 {
		t.Errorf("factors = overhead %v, replica %v, want 1.375 and 2", c.OverheadFactor, c.ReplicaFactor)
	}
	assertQty(t, "scaled", &c.Scaled, "414m") // ceil(150m x 110/80) = 207m, x 2
	assertQty(t, "coordinated", &c.Value, "414m")
	assertQty(t, "limit", cpu.Limit, "414m")
	assertQty(t, "request", res.Recs["app"].CPURequest, "414m")
	if cpu.OOMFloor != nil {
		t.Errorf("CPU has no OOM floor stage, got %+v", cpu.OOMFloor)
	}
}

// Coordination runs only when enabled and an autoscaler targets the
// workload. A resource the autoscaler has no target for gets overhead 1, and
// a kept request has no trace at all.
func TestCompute_TracesCoordinationOnlyWhenItRan(t *testing.T) {
	hpa := autoscaler.Info{Kind: autoscaler.KindHPA, ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 50}}
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 1},
		MemPerPod: promclient.ContainerValues{"app": 128 * mib},
	}
	compute := func(enabled bool, info autoscaler.Info, res sustainv1alpha1.ResourcesConfigs) sustainv1alpha1.ContainerTrace {
		return Compute(Request{
			Containers:   containers("app"),
			Inputs:       inputs,
			Resources:    res,
			Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: enabled},
			AutoInfo:     info,
		}).Containers["app"].Trace
	}

	if tr := compute(false, hpa, sustainv1alpha1.ResourcesConfigs{}); tr.CPU.Coordination != nil || tr.Memory.Coordination != nil {
		t.Errorf("coordination disabled, got %+v / %+v", tr.CPU.Coordination, tr.Memory.Coordination)
	}
	if tr := compute(true, autoscaler.Info{Kind: autoscaler.KindNone}, sustainv1alpha1.ResourcesConfigs{}); tr.CPU.Coordination != nil {
		t.Errorf("no autoscaler, got %+v", tr.CPU.Coordination)
	}

	tr := compute(true, hpa, sustainv1alpha1.ResourcesConfigs{})
	if c := tr.CPU.Coordination; c == nil || c.OverheadFactor != 2.2 || c.ReplicaFactor != nil {
		t.Errorf("cpu coordination = %+v, want overhead 2.2 and no replica correction", c)
	} else {
		assertQty(t, "cpu", &c.Value, "2200m")
	}
	if c := tr.Memory.Coordination; c == nil || c.OverheadFactor != 1 {
		t.Errorf("memory coordination = %+v, want overhead 1 without a memory target", c)
	} else {
		assertQty(t, "memory", &c.Value, "128Mi")
	}

	kept := compute(true, hpa, sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{
		Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
	}})
	if kept.Memory != nil || kept.CPU == nil {
		t.Errorf("trace = %+v, want CPU only when memory keeps its request", kept)
	}
}

// The limit stage records what was derived from the final request: a value,
// a removal, or nothing when the container keeps its own limit.
func TestCompute_TracesTheLimit(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.2},
			MemPerPod: promclient.ContainerValues{"app": 64 * mib},
		},
		Resources: sustainv1alpha1.ResourcesConfigs{
			CPU:    sustainv1alpha1.ResourceConfig{Limits: sustainv1alpha1.ResourceLimitsConfig{NoLimit: true}},
			Memory: sustainv1alpha1.ResourceConfig{Limits: sustainv1alpha1.ResourceLimitsConfig{RequestsLimitsRatio: ptr.To(2.0)}},
		},
	})

	tr := res.Containers["app"].Trace
	if !tr.CPU.RemoveLimit || tr.CPU.Limit != nil {
		t.Errorf("cpu limit = %v remove %v, want removed", tr.CPU.Limit, tr.CPU.RemoveLimit)
	}
	assertQty(t, "memory limit", tr.Memory.Limit, "128Mi")
	if rec := res.Recs["app"]; !rec.RemoveCPULimit || rec.MemoryLimit == nil || rec.MemoryLimit.Cmp(*tr.Memory.Limit) != 0 {
		t.Errorf("rec = %+v, want the limits the trace records", rec)
	}
}

func assertQty(t *testing.T, stage string, got *resource.Quantity, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %s", stage, want)
		return
	}
	if w := resource.MustParse(want); got.Cmp(w) != 0 {
		t.Errorf("%s = %s, want %s", stage, got, want)
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
