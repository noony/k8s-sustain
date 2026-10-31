package recommender

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// A span ends at the instant asked and reads at most maxHistoryPoints
// instants, at the finest whole multiple of the hint that allows it.
func TestSpanOf(t *testing.T) {
	cases := []struct {
		name         string
		over, hint   time.Duration
		wantStep     time.Duration
		wantInstants int
	}{
		{"a range the hint reads in few enough instants", time.Hour, time.Minute, time.Minute, 61},
		{"a week coarsened to a multiple of the hint", 7 * 24 * time.Hour, 20 * time.Minute, 40 * time.Minute, 253},
		{"no hint: whole seconds", 7 * 24 * time.Hour, 0, 2023 * time.Second, 299},
		{"no range: the one instant", 0, time.Minute, time.Minute, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := now.Add(-tc.over)

			s := SpanOf(start, now, tc.hint)

			if s.Step != tc.wantStep || s.instants() != tc.wantInstants {
				t.Errorf("step %s over %d instants, want %s over %d", s.Step, s.instants(), tc.wantStep, tc.wantInstants)
			}
			if !s.End.Equal(now) || s.Start.Before(start) || s.instants() > maxHistoryPoints {
				t.Errorf("span %v to %v, want it to end at %v, start no earlier than %v and read at most %d instants",
					s.Start, s.End, now, start, maxHistoryPoints)
			}
		})
	}
}

// One call reads an identity's history once, under its Request's resources,
// and runs the full chain at each instant on every signal's values as of it,
// the instant in each stage's context: the OOM floor raises the request only
// from the instant of the kill on, a limit-side signal raises the limit only
// where its values do, a container's own limit is the one it holds now, and
// the last instant computes what Compute does. The kills the history read
// come with the series.
func TestComputeRange_TheFullChainAtEachInstant(t *testing.T) {
	var seen []stageContext
	limiter := listedSignal{factor: 2}
	withSignals(t, cpuUsage, memoryUsage, oomFloor{}, limiter,
		probe{label: "probe", at: slot{role: adjuster, resource: cpuResource}, seen: &seen})
	key := prodID("api")
	t0, t1, t2 := now.Add(-2*time.Hour), now.Add(-time.Hour), now
	usage := func(cores float64) []InputsOption {
		return []InputsOption{
			WithCPUUsage(promclient.ContainerValues{"app": cores}),
			WithMemoryUsage(promclient.ContainerValues{"app": 100 * mib}),
		}
	}
	killed := NewInputs(append(usage(0.2), WithOOMKills(map[string]OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true, KillsInStep: 1}}))...)
	inputs := &recordedInputs{
		inputs: map[promclient.WorkloadIdentity]*WorkloadInputs{key: killed},
		histories: map[promclient.WorkloadIdentity]*History{key: NewHistory([]HistoryPoint{
			{At: t0, Inputs: NewInputs(usage(0.1)...)},
			{At: t1, Inputs: NewInputs(append(usage(0.2), setting(limiter, listedValues{"app": 128 * mib}))...)},
			{At: t2, Inputs: killed},
		})},
	}
	app := limited(nil, corev1.ResourceList{corev1.ResourceCPU: qty("1")})
	spec := sustainv1alpha1.RightSizingSpec{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
		Memory: sustainv1alpha1.ResourceConfig{Limits: sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true}},
	}}
	a := Assemble(context.Background(), inputs, &memberAutoscalers{}, func() time.Time { return now }, spec,
		[]*inventory.Identity{{Key: key, Containers: []corev1.Container{app}, Since: old}})[0]
	span := SpanOf(t0, t2, time.Hour)

	got, err := ComputeRange(context.Background(), inputs, a.Request, span)
	if err != nil {
		t.Fatalf("ComputeRange: %v", err)
	}

	if len(inputs.spans) != 1 || inputs.spans[0] != span || !reflect.DeepEqual(inputs.cfgs[1], a.Request.resources) ||
		!reflect.DeepEqual(inputs.calls[1], []InputsRequest{{Identity: key, Containers: 1}}) {
		t.Errorf("history reads %v under %+v for %v, want one over %v under the Request's resources, for the identity sized as assembled",
			inputs.spans, inputs.cfgs[1:], inputs.calls[1:], span)
	}
	points := func(vs ...float64) []promclient.TimeValue {
		out := make([]promclient.TimeValue, len(vs))
		for i, v := range vs {
			out[i] = promclient.TimeValue{Timestamp: []time.Time{t0, t1, t2}[i], Value: v}
		}
		return out
	}
	cpu, memory := got.Containers["app"][corev1.ResourceCPU], got.Containers["app"][corev1.ResourceMemory]
	for what, tc := range map[string]struct{ got, want []promclient.TimeValue }{
		"cpu usage":      {cpu.Usage, points(0.1, 0.2, 0.2)},
		"cpu request":    {cpu.Request, points(0.1, 0.2, 0.2)},
		"cpu limit":      {cpu.Limit, points(1, 1, 1)},
		"memory usage":   {memory.Usage, points(100*mib, 100*mib, 100*mib)},
		"memory request": {memory.Request, points(100*mib, 100*mib, 200*mib)},
		"memory limit":   {memory.Limit, points(100*mib, 256*mib, 200*mib)},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", what, tc.got, tc.want)
		}
	}
	if kills, want := OOMKills(got), []promclient.OOMKill{{Timestamp: t2, Container: "app"}}; !reflect.DeepEqual(kills, want) {
		t.Errorf("kills = %v, want %v", kills, want)
	}
	var instants []time.Time
	for _, sc := range seen {
		instants = append(instants, sc.now)
	}
	if want := []time.Time{t0, t0, t1, t1, t2, t2}; !reflect.DeepEqual(instants, want) {
		t.Errorf("stages ran as of %v, want each instant in turn", instants)
	}
	last := Compute(a.Request).Recs["app"]
	if float64(last.MemoryRequest.Value()) != memory.Request[2].Value || float64(last.MemoryLimit.Value()) != memory.Limit[2].Value {
		t.Errorf("last instant = %v / %v, want what Compute recommends as of it: %s / %s",
			memory.Request[2].Value, memory.Limit[2].Value, last.MemoryRequest, last.MemoryLimit)
	}
}

// A kept request has no series, and neither has its limit: nothing is
// recommended for that resource.
func TestComputeRange_AKeptRequestHasNoSeries(t *testing.T) {
	key := prodID("api")
	inputs := &recordedInputs{histories: map[promclient.WorkloadIdentity]*History{key: NewHistory([]HistoryPoint{
		{At: now, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.1}), WithMemoryUsage(promclient.ContainerValues{"app": 64 * mib}))},
	})}}
	spec := sustainv1alpha1.RightSizingSpec{ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
		Memory: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true}},
	}}
	a := Assemble(context.Background(), inputs, &memberAutoscalers{}, func() time.Time { return now }, spec,
		[]*inventory.Identity{{Key: key, Containers: containers("app"), Since: old}})[0]

	got, err := ComputeRange(context.Background(), inputs, a.Request, SpanOf(now, now, time.Minute))
	if err != nil {
		t.Fatalf("ComputeRange: %v", err)
	}

	mem := got.Containers["app"][corev1.ResourceMemory]
	if len(mem.Usage) != 1 || mem.Request != nil || mem.Limit != nil || len(got.Containers["app"][corev1.ResourceCPU].Request) != 1 {
		t.Errorf("app = %+v, want memory usage only and a CPU request", got.Containers["app"])
	}
}

// A history that cannot be read is ComputeRange's error.
func TestComputeRange_AnUnreadableHistoryIsItsError(t *testing.T) {
	key := prodID("api")
	down := errors.New("prometheus down")
	reader := &recordedInputs{}
	a := Assemble(context.Background(), reader, &memberAutoscalers{}, func() time.Time { return now },
		sustainv1alpha1.RightSizingSpec{}, []*inventory.Identity{{Key: key, Containers: containers("app"), Since: old}})[0]
	reader.failed = map[promclient.WorkloadIdentity]error{key: down}

	if _, err := ComputeRange(context.Background(), reader, a.Request, SpanOf(old, now, time.Minute)); !errors.Is(err, down) {
		t.Errorf("err = %v, want %v", err, down)
	}
}

// ReadRange reads the history once, under the spec's resources with every
// default explicit, and returns what the signals read at each instant: the
// usage of each container the spec recommends, none for an init container it
// excludes, none for a resource whose request it keeps, and nothing computed,
// with the kills.
func TestReadRange_WhatTheSignalsReadAtEachInstant(t *testing.T) {
	key := prodID("api")
	t0, t1 := now.Add(-time.Hour), now
	read := func(cores float64, opts ...InputsOption) *WorkloadInputs {
		return NewInputs(append(opts,
			WithCPUUsage(promclient.ContainerValues{"app": cores, "migrate": 2}),
			WithMemoryUsage(promclient.ContainerValues{"app": 64 * mib, "migrate": 32 * mib}),
		)...)
	}
	inputs := &recordedInputs{histories: map[promclient.WorkloadIdentity]*History{key: NewHistory([]HistoryPoint{
		{At: t0, Inputs: read(0.1)},
		{At: t1, Inputs: read(0.3, WithOOMKills(map[string]OOM{"app": {Kills: 1, KillsInStep: 1}}))},
	})}}
	spec := sustainv1alpha1.RightSizingSpec{
		ExcludeInitContainers: true,
		ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
			CPU:    sustainv1alpha1.ResourceConfig{Window: "24h"},
			Memory: sustainv1alpha1.ResourceConfig{Requests: sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true}},
		},
	}
	id := &inventory.Identity{Key: key, Containers: containers("app"), InitContainers: containers("migrate"), Since: old}
	span := SpanOf(t0, t1, time.Hour)

	got, err := ReadRange(context.Background(), inputs, spec, id, span)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}

	if len(inputs.spans) != 1 || inputs.spans[0] != span || !reflect.DeepEqual(inputs.cfgs, []sustainv1alpha1.ResourcesConfigs{WithDefaults(spec.ResourcesConfigs)}) ||
		!reflect.DeepEqual(inputs.calls, [][]InputsRequest{{{Identity: key, Containers: 1}}}) || len(inputs.ats) != 0 {
		t.Errorf("reads %v under %+v for %v, instant reads %v; want one history over %v under the defaulted resources, for the containers recommended",
			inputs.spans, inputs.cfgs, inputs.calls, inputs.ats, span)
	}
	want := map[string]ContainerRange{"app": {corev1.ResourceCPU: {Usage: []promclient.TimeValue{{Timestamp: t0, Value: 0.1}, {Timestamp: t1, Value: 0.3}}}}}
	if !reflect.DeepEqual(got.Containers, want) {
		t.Errorf("containers = %+v, want %+v", got.Containers, want)
	}
	if kills, want := OOMKills(got), []promclient.OOMKill{{Timestamp: t1, Container: "app"}}; !reflect.DeepEqual(kills, want) {
		t.Errorf("kills = %v, want %v", kills, want)
	}
}

// An identity with no containers at all has a series for every container its
// history reports, and a history that cannot be read is the error.
func TestReadRange_AnUnseenIdentityAndAnUnreadableHistory(t *testing.T) {
	seen, down := prodID("seen"), prodID("down")
	failure := errors.New("prometheus down")
	inputs := &recordedInputs{
		histories: map[promclient.WorkloadIdentity]*History{seen: NewHistory([]HistoryPoint{
			{At: now, Inputs: NewInputs(WithMemoryUsage(promclient.ContainerValues{"app": 64 * mib, "sidecar": 8 * mib}))},
		})},
		failed: map[promclient.WorkloadIdentity]error{down: failure},
	}
	span := SpanOf(now, now, time.Minute)

	got, err := ReadRange(context.Background(), inputs, sustainv1alpha1.RightSizingSpec{}, &inventory.Identity{Key: seen}, span)
	if err != nil || len(got.Containers) != 2 ||
		len(got.Containers["app"][corev1.ResourceMemory].Usage) != 1 || len(got.Containers["sidecar"][corev1.ResourceMemory].Usage) != 1 {
		t.Errorf("containers = %+v, %v; want app's and sidecar's memory", got.Containers, err)
	}
	if _, err := ReadRange(context.Background(), inputs, sustainv1alpha1.RightSizingSpec{}, &inventory.Identity{Key: down}, span); !errors.Is(err, failure) {
		t.Errorf("err = %v, want %v", err, failure)
	}
}
