package recommender

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

const mib = float64(mebibyte)

// now is the instant every test computes as of.
var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// old dates an identity well past the age gate.
var old = now.Add(-time.Hour)

func containers(names ...string) []corev1.Container {
	out := make([]corev1.Container, 0, len(names))
	for _, n := range names {
		out = append(out, corev1.Container{Name: n})
	}
	return out
}

// limited is a container named app running with the given requests and
// limits.
func limited(requests, limits corev1.ResourceList) corev1.Container {
	return corev1.Container{Name: "app", Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits}}
}

// Declared containers bound the result: one Prometheus still reports but the
// workload no longer runs gets nothing, and one with no signal is skipped
// rather than recommended at the hard floor.
func TestCompute_DeclaredContainersBoundTheResult(t *testing.T) {
	res := Compute(Request{
		containers: containers("app", "nodata"),
		inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 1, "renamed-away": 1},
			MemPerPod: promclient.ContainerValues{"app": 64 * mib},
		},
		now: now,
	})

	if len(res.Recs) != 1 || len(res.Traces) != 1 {
		t.Fatalf("recs = %v, traces = %v, want app only", res.Recs, res.Traces)
	}
	if rec := res.Recs["app"]; rec.CPURequest == nil || rec.MemoryRequest == nil {
		t.Errorf("app = %+v, want CPU and memory requests", rec)
	}
}

// At the usage a container was observed at, RequestAt is the request Compute
// recommends it, for either resource, its own OOM floor included and a
// sibling's left out. It is zero for a container the Request does not
// recommend, a kept request, and a resource the recommender does not size.
func TestRequest_RequestAt(t *testing.T) {
	req := Request{
		containers: containers("app", "side"),
		resources: sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{
			Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](50)},
		}},
		inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 1, "side": 0.2},
			MemPerPod: promclient.ContainerValues{"app": 64 * mib, "side": 32 * mib},
			OOM:       map[string]OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}},
		},
		since: now.Add(-time.Minute),
		now:   now,
	}
	whole := Compute(req)
	kept := req
	kept.resources.Memory.Requests.KeepRequest = true

	for _, name := range []string{"app", "side"} {
		rec := whole.Recs[name]
		if got, want := req.RequestAt(name, corev1.ResourceCPU, req.inputs.CPUPerPod[name]), float64(rec.CPURequest.MilliValue())/1000; got != want {
			t.Errorf("%s cpu = %v, want Compute's %v", name, got, want)
		}
		if got, want := req.RequestAt(name, corev1.ResourceMemory, req.inputs.MemPerPod[name]), float64(rec.MemoryRequest.Value()); got != want {
			t.Errorf("%s memory = %v, want Compute's %v", name, got, want)
		}
	}
	for what, got := range map[string]float64{
		"an absent container": req.RequestAt("absent", corev1.ResourceCPU, 1),
		"a kept request":      kept.RequestAt("app", corev1.ResourceMemory, 64*mib),
		"an unsized resource": req.RequestAt("app", corev1.ResourceEphemeralStorage, 1),
	} {
		if got != 0 {
			t.Errorf("%s = %v, want 0", what, got)
		}
	}
}

// The age gate holds an identity first seen less than MinWorkloadAge before
// the instant computed as of, and any recent OOM, from Prometheus or the live
// watcher, excuses it so a crash-looping workload is not locked out.
func TestCompute_AgeGate(t *testing.T) {
	cases := []struct {
		name         string
		since        time.Time
		oom          *OOM
		wantTooYoung bool
	}{
		{"young identity", now.Add(-time.Minute), nil, true},
		{"just old enough", now.Add(-MinWorkloadAge), nil, false},
		{"old identity", now.Add(-2 * MinWorkloadAge), nil, false},
		{"young identity, Prometheus OOM excuses", now.Add(-time.Minute), &OOM{Kills: 1}, false},
		{"young identity, live OOM excuses", now.Add(-time.Minute), &OOM{LiveAt: now}, false},
		{"young identity, OOM long ago does not", now.Add(-time.Minute), &OOM{PeakBytes: 1, HasPeak: true}, true},
		{"undated identity", time.Time{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 1}}
			if tc.oom != nil {
				in.OOM = map[string]OOM{"side": *tc.oom}
			}

			res := Compute(Request{containers: containers("app"), inputs: in, since: tc.since, now: now})

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
func computeApp(in *WorkloadInputs, rs sustainv1alpha1.ResourcesConfigs) (workload.ContainerRecommendation, sustainv1alpha1.ContainerTrace) {
	res := Compute(Request{containers: containers("app"), inputs: in, resources: rs, now: now})
	return res.Recs["app"], res.Traces["app"]
}

func TestCompute_NoDataWhenNothingToRecommendFrom(t *testing.T) {
	res := Compute(Request{
		containers: containers("app"),
		inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{}, MemPerPod: promclient.ContainerValues{}},
		since:      old,
		now:        now,
	})

	if res.Outcome != NoData {
		t.Errorf("outcome = %v, want NoData", res.Outcome)
	}
	if len(res.Recs) != 0 {
		t.Errorf("recs = %v, want none", res.Recs)
	}
}

// The CPU trace lists every stage that ran, in order, with the value after
// it: the percentile at full precision, headroom rounded up, a minAllowed
// clamp moving it, coordination with both factors, then the limit strategy.
func TestCompute_TracesCPUStages(t *testing.T) {
	anchor := 0.0
	res := Compute(Request{
		containers: containers("app"),
		inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 0.1234}},
		resources: sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{
			Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](10), MinAllowed: qtyp("150m")},
			Limits:   sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true},
		}},
		coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor},
		autoInfo: autoscaler.Info{
			Kind:              autoscaler.KindHPA,
			MinReplicas:       2,
			MaxReplicas:       10,
			CurrentReplicas:   8, // 4x the anchor target, factor clamps to 2.0
			ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 80},
		},
		now: now,
	})

	cpu := res.Traces["app"].CPU
	if cpu == nil {
		t.Fatal("no CPU trace")
	}
	assertStages(t, "request", cpu.Request, "percentile", "headroom", "clamp", "coordination")
	assertQty(t, "percentile", cpu.Request[0].Value, "123400u")
	assertQty(t, "headroom", cpu.Request[1].Value, "136m")     // ceil(123.4m x 1.1)
	assertQty(t, "clamp", cpu.Request[2].Value, "150m")        // raised to minAllowed
	assertQty(t, "coordination", cpu.Request[3].Value, "414m") // ceil(150m x 110/80) = 207m, x 2
	assertFacts(t, cpu.Request[3], map[string]string{"overheadFactor": "1.375", "replicaFactor": "2"})
	assertStages(t, "limit", cpu.Limit, "equalsToRequest")
	assertQty(t, "limit", cpu.Limit[0].Value, "414m")
	assertQty(t, "request", res.Recs["app"].CPURequest, "414m")
	assertQty(t, "recommended limit", res.Recs["app"].CPULimit, "414m")
}

// Coordination runs only when enabled and an autoscaler targets the
// workload. A resource the autoscaler has no target for gets overhead 1, a
// clamp after coordination keeps what the factors produced as a fact, and a
// kept request has no trace at all.
func TestCompute_TracesCoordinationOnlyWhenItRan(t *testing.T) {
	hpa := autoscaler.Info{Kind: autoscaler.KindHPA, ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 50}}
	inputs := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 1},
		MemPerPod: promclient.ContainerValues{"app": 128 * mib},
	}
	compute := func(enabled bool, info autoscaler.Info, res sustainv1alpha1.ResourcesConfigs) sustainv1alpha1.ContainerTrace {
		return Compute(Request{
			containers:   containers("app"),
			inputs:       inputs,
			resources:    res,
			coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: enabled},
			autoInfo:     info,
			now:          now,
		}).Traces["app"]
	}

	for name, tr := range map[string]sustainv1alpha1.ContainerTrace{
		"disabled":      compute(false, hpa, sustainv1alpha1.ResourcesConfigs{}),
		"no autoscaler": compute(true, autoscaler.Info{Kind: autoscaler.KindNone}, sustainv1alpha1.ResourcesConfigs{}),
	} {
		if c, ok := TracedCoordination(tr.CPU); ok {
			t.Errorf("%s: coordination = %+v, want none", name, c)
		}
		assertStages(t, name, tr.CPU.Request, "percentile", "headroom", "clamp")
	}

	tr := compute(true, hpa, sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{
		Requests: sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("2")},
	}})
	if c, ok := TracedCoordination(tr.CPU); !ok || c != (Coordination{Overhead: 2.2}) {
		t.Errorf("cpu coordination = %+v, want overhead 2.2 and no replica correction", c)
	}
	coordination := tr.CPU.Request[len(tr.CPU.Request)-1]
	assertQty(t, "cpu", coordination.Value, "2")
	assertFacts(t, coordination, map[string]string{"overheadFactor": "2.2", "scaled": "2200m"})
	if c, ok := TracedCoordination(tr.Memory); !ok || c != (Coordination{Overhead: 1}) {
		t.Errorf("memory coordination = %+v, want overhead 1 without a memory target", c)
	}

	kept := compute(true, hpa, sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{
		Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
	}})
	if kept.Memory != nil || kept.CPU == nil {
		t.Errorf("trace = %+v, want CPU only when memory keeps its request", kept)
	}
}

func TestAgeForLog(t *testing.T) {
	if got := AgeForLog(time.Time{}, now); got != "none" {
		t.Errorf("AgeForLog(zero) = %q, want %q", got, "none")
	}
	if got := AgeForLog(now.Add(-30*time.Minute), now); got != "30m0s" {
		t.Errorf("AgeForLog(30m ago) = %q, want 30m0s", got)
	}
}

// computeLimited computes the CPU of c from a usage of cores under the limit
// strategy cfg.
func computeLimited(t *testing.T, c corev1.Container, cores float64, cfg sustainv1alpha1.ResourceLimitsConfig) (workload.ContainerRecommendation, *sustainv1alpha1.ResourceTrace) {
	t.Helper()
	res := Compute(Request{
		containers: []corev1.Container{c},
		inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{c.Name: cores}},
		resources:  sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Limits: cfg}},
		now:        now,
	})
	tr := res.Traces[c.Name].CPU
	if tr == nil {
		t.Fatalf("no CPU trace for %s", c.Name)
	}
	return res.Recs[c.Name], tr
}

func assertStages(t *testing.T, what string, stages []sustainv1alpha1.TraceStage, want ...string) {
	t.Helper()
	got := make([]string, len(stages))
	for i, st := range stages {
		got[i] = st.Stage
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s stages = %v, want %v", what, got, want)
	}
}

func assertFacts(t *testing.T, st sustainv1alpha1.TraceStage, want map[string]string) {
	t.Helper()
	if len(st.Facts) != len(want) {
		t.Errorf("%s facts = %v, want %v", st.Stage, st.Facts, want)
		return
	}
	for k, v := range want {
		if st.Facts[k] != v {
			t.Errorf("%s facts = %v, want %v", st.Stage, st.Facts, want)
			return
		}
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
