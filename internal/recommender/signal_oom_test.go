package recommender

import (
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// liveKill is a kill the OOM Pod watcher saw ago, at limitBytes.
func liveKill(container string, ago time.Duration, limitBytes int64) *oomwatch.OOMRecord {
	at := time.Now().Add(-ago)
	return &oomwatch.OOMRecord{Container: container, ObservedAt: at, TerminatedAt: at, OOMLimitBytes: limitBytes}
}

// The floor reads its three 24h rules in one query, selecting them and the
// shard's owner names as exact alternations.
func TestOOMFloor_QueryReadsItsRulesForTheShard(t *testing.T) {
	want := `{__name__=~"k8s_sustain:workload_oom_24h|k8s_sustain:container_peak_memory_24h:bytes|k8s_sustain:container_oom_limit_24h:bytes",` +
		`namespace="prod",owner_kind="Deployment",owner_name=~"payments\\.worker|api"}`

	if got := (oomFloor{}).query(sustainv1alpha1.ResourcesConfigs{}, dottedShard); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// The rules are aggregated over 24h and read as an instant vector, so a
// container costs one sample per rule however long the Policy's windows are.
func TestOOMFloor_ShardCostIgnoresTheWindows(t *testing.T) {
	month := sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{Window: "720h"},
		Memory: sustainv1alpha1.ResourceConfig{Window: "720h"},
	}

	if got := (oomFloor{}).samplesPerContainer(month); got != 3 {
		t.Errorf("cost = %d, want 3, one sample per rule", got)
	}
}

// The floor only raises a request: an identity whose OOM data cannot be read
// is computed without it rather than not at all.
func TestOOMFloor_IsBestEffort(t *testing.T) {
	if (oomFloor{}).required() {
		t.Error("the OOM floor is required, want best-effort")
	}
}

// Two kube-state-metrics replicas duplicate every series: kills sum, peaks and
// limits take the max. A zero peak is still a peak, and a container with a
// peak but no kill was not killed.
func TestOOMFloor_CollectFoldsEachContainer(t *testing.T) {
	samples := []promclient.ShardSample{
		{Container: "app", Metric: promclient.MetricWorkloadOOM24h, Value: 2},
		{Container: "app", Metric: promclient.MetricWorkloadOOM24h, Value: 3},
		{Container: "app", Metric: promclient.MetricContainerPeakMemory24hBytes, Value: 1000},
		{Container: "app", Metric: promclient.MetricContainerPeakMemory24hBytes, Value: 2000},
		{Container: "app", Metric: promclient.MetricContainerOOMLimit24hBytes, Value: 4096},
		{Container: "app", Metric: "k8s_sustain:unrelated", Value: 99},
		{Container: "sidecar", Metric: promclient.MetricContainerPeakMemory24hBytes, Value: 0},
	}
	in := &WorkloadInputs{}

	oomFloor{}.collect(in, samples)

	want := map[string]OOM{
		"app":     {Kills: 5, PeakBytes: 2000, HasPeak: true, LimitBytes: 4096},
		"sidecar": {HasPeak: true},
	}
	if !maps.Equal(in.OOM, want) {
		t.Errorf("OOM = %+v, want %+v", in.OOM, want)
	}
	if got := (oomFloor{}).observed(in); !slices.Equal(got, []string{"app"}) {
		t.Errorf("observed = %v, want the killed container only", got)
	}

	oomFloor{}.collect(in, nil)

	if in.OOM == nil || len(in.OOM) != 0 {
		t.Errorf("OOM = %v, want empty and non-nil after no samples", in.OOM)
	}
}

// A recently killed container's floor is the higher of its peak and the limit
// it was killed at bumped by 1.20. It justifies a request on its own only with
// one of those anchors; a container that was not killed has no floor.
func TestOOMFloor_Stage(t *testing.T) {
	cases := []struct {
		name        string
		oom         OOM
		wantOK      bool
		want        float64
		wantAnchors bool
	}{
		{"not killed", OOM{PeakBytes: 300 * mib, HasPeak: true}, false, 0, false},
		{"peak", OOM{Kills: 1, PeakBytes: 200 * mib, HasPeak: true}, true, 200 * mib, true},
		{"bumped limit beats an unreliable peak", OOM{Kills: 1, PeakBytes: 36 * mib, HasPeak: true, LimitBytes: 100 * mib}, true, 120 * mib, true},
		{"peak beats the bumped limit", OOM{Kills: 1, PeakBytes: 300 * mib, HasPeak: true, LimitBytes: 96 * mib}, true, 300 * mib, true},
		{"live kill at a limit", OOM{LiveAt: time.Now(), LimitBytes: 100 * mib}, true, 120 * mib, true},
		{"zero peak", OOM{Kills: 1, HasPeak: true}, true, 0, true},
		{"no anchor", OOM{Kills: 1}, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := oomFloor{}.contribute(&WorkloadInputs{OOM: map[string]OOM{"app": tc.oom}}, "app")

			if ok != tc.wantOK {
				t.Fatalf("contributed = %v, want %v", ok, tc.wantOK)
			}
			if math.Abs(c.value-tc.want) > 1 || c.anchors != tc.wantAnchors {
				t.Errorf("contribution = %+v, want value %v anchoring %v", c, tc.want, tc.wantAnchors)
			}
		})
	}
}

func TestOOMFloor_RecordsTheFloorAndWhetherItDetermined(t *testing.T) {
	var tr sustainv1alpha1.ResourceTrace

	oomFloor{}.record(&tr, 200*mib, true)

	if tr.OOMFloor == nil || !tr.OOMFloor.Determined {
		t.Fatalf("oomFloor = %+v, want a determining floor", tr.OOMFloor)
	}
	assertQty(t, "oomFloor", &tr.OOMFloor.Value, "200Mi")
}

// A kill the watcher saw within LiveOOMWindow counts before the recording
// rules surface it: it lets a Too young identity through the age gate and
// floors the killed container above its limit at the kill. Only that
// container is floored.
func TestOOMFloor_LiveKillWithinTheWindow(t *testing.T) {
	kill := liveKill("app", 10*time.Second, 200<<20)

	res := Compute(Request{
		Containers: containers("app", "side"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.1, "side": 0.05},
			MemPerPod: promclient.ContainerValues{"app": 100 * mib, "side": 100 * mib},
		},
		LiveOOMs: map[string]*oomwatch.OOMRecord{"app": kill},
		Since:    time.Now().Add(-time.Minute),
	})

	if res.Outcome != Recommended {
		t.Fatalf("outcome = %v, want Recommended: a live kill bypasses the age gate", res.Outcome)
	}
	if got := res.Recs["app"].MemoryRequest; got == nil || got.String() != "240Mi" {
		t.Errorf("app memory = %v, want 240Mi (200Mi limit at the kill x 1.2)", got)
	}
	app := res.Containers["app"]
	if floor := app.Trace.Memory.OOMFloor; floor == nil || !floor.Determined {
		t.Errorf("app: oomFloor = %+v, want it to have determined the request", floor)
	}
	if !app.OOM.LiveAt.Equal(kill.TerminatedAt) {
		t.Errorf("app: OOM.LiveAt = %v, want the kill time %v", app.OOM.LiveAt, kill.TerminatedAt)
	}
	if got := res.Recs["side"].MemoryRequest; got == nil || got.String() != "100Mi" {
		t.Errorf("side memory = %v, want its 100Mi percentile: a sibling's kill must not floor it", got)
	}
	if floor := res.Containers["side"].Trace.Memory.OOMFloor; floor != nil {
		t.Errorf("side: oomFloor = %+v for a container that was not killed", floor)
	}
}

// A kill the watcher saw longer than LiveOOMWindow ago is no longer recent:
// it neither excuses the age gate nor floors the container.
func TestOOMFloor_StaleLiveKillIsIgnored(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.1},
			MemPerPod: promclient.ContainerValues{"app": 100 * mib},
		},
		LiveOOMs: map[string]*oomwatch.OOMRecord{"app": liveKill("app", LiveOOMWindow+time.Minute, 200<<20)},
		Since:    time.Now().Add(-time.Minute),
	})

	if res.Outcome != TooYoung {
		t.Errorf("outcome = %v, want TooYoung: a stale kill must not bypass the age gate", res.Outcome)
	}
	app := res.Containers["app"]
	if got := res.Recs["app"].MemoryRequest; got == nil || got.String() != "100Mi" {
		t.Errorf("app memory = %v, want its 100Mi percentile", got)
	}
	if app.Trace.Memory.OOMFloor != nil || !app.OOM.LiveAt.IsZero() {
		t.Errorf("app: oomFloor = %+v, LiveAt = %v, want no trace of the stale kill", app.Trace.Memory.OOMFloor, app.OOM.LiveAt)
	}
}

// Kills in the recording rules' window alone let a Too young identity through
// and floor the killed container at its peak: the bypass is identity-wide,
// the floor per container.
func TestOOMFloor_PrometheusKills(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app", "side"),
		Inputs:     &WorkloadInputs{OOM: map[string]OOM{"app": {Kills: 1, PeakBytes: 80 * mib, HasPeak: true}}},
		Since:      time.Now().Add(-time.Minute),
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

// Prometheus's OOM-time limit is windowed and can still report the limit from
// before a resize; the live record has the limit applied at the kill. The
// higher one is the limit the container died at.
func TestOOMFloor_AnchorsOnTheHigherOfPrometheusAndLiveLimits(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app"),
		Inputs: &WorkloadInputs{
			CPUPerPod: promclient.ContainerValues{"app": 0.1},
			MemPerPod: promclient.ContainerValues{"app": 50 * mib},
			OOM:       map[string]OOM{"app": {Kills: 1, LimitBytes: 96 * mib}},
		},
		LiveOOMs: map[string]*oomwatch.OOMRecord{"app": liveKill("app", 0, 184<<20)},
		Since:    old(),
	})

	if got := res.Recs["app"].MemoryRequest; got == nil || got.Value() <= 184<<20 {
		t.Errorf("app memory = %v, want a bump above the live 184Mi, not the stale 96Mi", got)
	}
}

// The peak rule is not OOM-scoped: every container has a 24h high-water mark.
// Only the container that OOMed is floored at it; an innocent sibling keeps its
// percentile and one with no usage gets nothing.
func TestOOMFloor_SiblingKillDoesNotFloorInnocentContainer(t *testing.T) {
	res := Compute(Request{
		Containers: containers("app", "side", "nodata"),
		Inputs: &WorkloadInputs{
			MemPerPod: promclient.ContainerValues{"app": 64 * mib, "side": 50 * mib},
			OOM: map[string]OOM{
				"app":    {Kills: 2, PeakBytes: 200 * mib, HasPeak: true},
				"side":   {PeakBytes: 180 * mib, HasPeak: true},
				"nodata": {PeakBytes: 150 * mib, HasPeak: true},
			},
		},
	})

	if got := res.Recs["app"].MemoryRequest; got == nil || got.String() != "200Mi" {
		t.Errorf("app memory = %v, want 200Mi (peak floor)", got)
	}
	if floor := res.Containers["app"].Trace.Memory.OOMFloor; floor == nil || !floor.Determined {
		t.Errorf("app: oomFloor = %+v, want it to have determined the request", floor)
	}
	if got := res.Recs["side"].MemoryRequest; got == nil || got.String() != "50Mi" {
		t.Errorf("side memory = %v, want its 50Mi percentile", got)
	}
	if floor := res.Containers["side"].Trace.Memory.OOMFloor; floor != nil {
		t.Errorf("side: oomFloor = %+v, the floor must not apply to a container that did not OOM", floor)
	}
	if _, ok := res.Recs["nodata"]; ok {
		t.Errorf("nodata: no OOM and no usage must yield nothing, got %+v", res.Recs["nodata"])
	}
}

// A live kill with no anchor at all (no usage, no peak, no OOM-time limit)
// emits nothing: the only possible value is the 1Mi floor, which guarantees
// the next kill. With the OOM-time limit as its only anchor it bumps above it.
func TestOOMFloor_LiveKillNeedsAnAnchor(t *testing.T) {
	empty := &WorkloadInputs{CPUPerPod: promclient.ContainerValues{}, MemPerPod: promclient.ContainerValues{}}

	none := Compute(Request{
		Containers: containers("app"),
		Inputs:     empty,
		LiveOOMs:   map[string]*oomwatch.OOMRecord{"app": liveKill("app", 0, 0)},
	})
	if _, ok := none.Recs["app"]; ok {
		t.Errorf("live OOM with no anchor must not emit a recommendation, got %v", none.Recs)
	}

	limit := Compute(Request{
		Containers: containers("app"),
		Inputs:     empty,
		LiveOOMs:   map[string]*oomwatch.OOMRecord{"app": liveKill("app", 0, 100<<20)},
	})
	if got := limit.Recs["app"].MemoryRequest; got == nil || got.String() != "120Mi" {
		t.Errorf("app memory = %v, want 120Mi (100Mi limit x 1.2)", got)
	}
}

// The floor is applied before headroom, once, and an operator bound still
// wins over it.
func TestOOMFloor_FloorsTheMemoryRequest(t *testing.T) {
	tests := []struct {
		name     string
		rawBytes float64
		oom      OOM
		cfg      sustainv1alpha1.ResourceRequestsConfig
		wantNil  bool
		want     string
	}{
		{"no recent OOM keeps the percentile", 100 * mib, OOM{PeakBytes: 300 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{}, false, "100Mi"},
		{"recent OOM raises the request to the peak", 50 * mib, OOM{Kills: 1, PeakBytes: 200 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{}, false, "200Mi"},
		{"a percentile above the floor wins", 300 * mib, OOM{Kills: 1, PeakBytes: 100 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{}, false, "300Mi"},
		// raw=50Mi → 60Mi; peak=100Mi → 120Mi; max(60, 120) = 120Mi.
		{"headroom applied to the floor once", 50 * mib, OOM{Kills: 1, PeakBytes: 100 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](20)}, false, "120Mi"},
		{"maxAllowed wins over the floor", 50 * mib, OOM{Kills: 1, PeakBytes: 500 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("256Mi")}, false, "256Mi"},
		{"a kept request stays kept", 50 * mib, OOM{Kills: 1, PeakBytes: 200 * mib, HasPeak: true}, sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeApp(&WorkloadInputs{
				MemPerPod: promclient.ContainerValues{"app": tc.rawBytes},
				OOM:       map[string]OOM{"app": tc.oom},
			}, sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{Requests: tc.cfg}}).Rec.MemoryRequest

			if tc.wantNil {
				if got != nil {
					t.Errorf("memory = %s, want none", got)
				}
				return
			}
			assertQty(t, "memory", got, tc.want)
		})
	}
}

// The trace says the OOM floor determined the request only when the floor
// produced the final value: it beat the percentile and no operator bound
// replaced it.
func TestOOMFloor_TracesWhetherItDetermined(t *testing.T) {
	killed := OOM{Kills: 1, PeakBytes: 200 * mib, HasPeak: true}
	tests := []struct {
		name      string
		rawBytes  float64
		oom       OOM
		cfg       sustainv1alpha1.ResourceRequestsConfig
		wantFloor bool
		want      bool
	}{
		{"floor beats the percentile", 50 * mib, killed, sustainv1alpha1.ResourceRequestsConfig{}, true, true},
		{"percentile beats the floor", 400 * mib, killed, sustainv1alpha1.ResourceRequestsConfig{}, true, false},
		{"no recent OOM has no floor", 50 * mib, OOM{}, sustainv1alpha1.ResourceRequestsConfig{}, false, false},
		{"maxAllowed below the floor", 50 * mib, killed, sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("128Mi")}, true, false},
		{"maxAllowed above the floor", 50 * mib, killed, sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("1Gi")}, true, true},
		{"minAllowed above the floor", 50 * mib, killed, sustainv1alpha1.ResourceRequestsConfig{MinAllowed: qtyp("512Mi")}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			floor := computeApp(&WorkloadInputs{
				MemPerPod: promclient.ContainerValues{"app": tc.rawBytes},
				OOM:       map[string]OOM{"app": tc.oom},
			}, sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{Requests: tc.cfg}}).Trace.Memory.OOMFloor

			if (floor != nil) != tc.wantFloor {
				t.Fatalf("oomFloor = %+v, want present %v", floor, tc.wantFloor)
			}
			if floor != nil && floor.Determined != tc.want {
				t.Errorf("determined = %v, want %v", floor.Determined, tc.want)
			}
		})
	}
}

// Coordination is judged on the final request: the floor determined it after
// an overhead that only scaled it, but not after a maxAllowed re-clamp
// replaced it, nor after a minAllowed clamp before coordination did.
func TestOOMFloor_TracesThroughCoordination(t *testing.T) {
	cases := []struct {
		name           string
		requests       sustainv1alpha1.ResourceRequestsConfig
		wantClamped    string
		wantScaled     string
		wantRequest    string
		wantDetermined bool
	}{
		{"overhead only scales the floor", sustainv1alpha1.ResourceRequestsConfig{}, "200Mi", "275Mi", "275Mi", true},
		{"maxAllowed re-clamps after coordination", sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("256Mi")}, "200Mi", "275Mi", "256Mi", false},
		{"minAllowed clamps before coordination", sustainv1alpha1.ResourceRequestsConfig{MinAllowed: qtyp("240Mi")}, "240Mi", "330Mi", "330Mi", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Compute(Request{
				Containers: containers("app"),
				Inputs: &WorkloadInputs{
					MemPerPod: promclient.ContainerValues{"app": 100 * mib},
					OOM:       map[string]OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}},
				},
				Resources:    sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{Requests: tc.requests}},
				Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true},
				AutoInfo:     autoscaler.Info{Kind: autoscaler.KindKEDA, ConfiguredTargets: map[string]int32{autoscaler.ResourceMemory: 80}},
				Since:        old(),
			})

			mem := res.Containers["app"].Trace.Memory
			if mem == nil || mem.OOMFloor == nil || mem.Coordination == nil {
				t.Fatalf("memory trace = %+v, want percentile, OOM floor and coordination stages", mem)
			}
			assertQty(t, "percentile", mem.Percentile, "100Mi")
			assertQty(t, "oomFloor", &mem.OOMFloor.Value, "200Mi")
			assertQty(t, "withHeadroom", &mem.WithHeadroom, "200Mi")
			assertQty(t, "clamped", &mem.Clamped, tc.wantClamped)
			assertQty(t, "scaled", &mem.Coordination.Scaled, tc.wantScaled)
			assertQty(t, "request", res.Recs["app"].MemoryRequest, tc.wantRequest)
			if mem.Coordination.ReplicaFactor != nil {
				t.Errorf("memory has no replica correction, got %v", *mem.Coordination.ReplicaFactor)
			}
			if mem.OOMFloor.Determined != tc.wantDetermined {
				t.Errorf("determined = %v, want %v", mem.OOMFloor.Determined, tc.wantDetermined)
			}
		})
	}
}
