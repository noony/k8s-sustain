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
// the last instant computes what Compute does. The kills come with the
// series.
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
	kill := promclient.OOMKill{Timestamp: t2, Container: "app"}
	killed := NewInputs(append(usage(0.2), WithOOMKills(map[string]OOM{"app": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true}}))...)
	inputs := &recordedInputs{
		inputs: map[promclient.WorkloadIdentity]*WorkloadInputs{key: killed},
		histories: map[promclient.WorkloadIdentity]*History{key: NewHistory([]HistoryPoint{
			{At: t0, Inputs: NewInputs(usage(0.1)...)},
			{At: t1, Inputs: NewInputs(append(usage(0.2), setting(limiter, listedValues{"app": 128 * mib}))...)},
			{At: t2, Inputs: killed},
		}, kill)},
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
	c := got.Containers["app"]
	for what, tc := range map[string]struct{ got, want []promclient.TimeValue }{
		"cpu usage":      {c.CPU.Usage, points(0.1, 0.2, 0.2)},
		"cpu request":    {c.CPU.Request, points(0.1, 0.2, 0.2)},
		"cpu limit":      {c.CPU.Limit, points(1, 1, 1)},
		"memory usage":   {c.Memory.Usage, points(100*mib, 100*mib, 100*mib)},
		"memory request": {c.Memory.Request, points(100*mib, 100*mib, 200*mib)},
		"memory limit":   {c.Memory.Limit, points(100*mib, 256*mib, 200*mib)},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", what, tc.got, tc.want)
		}
	}
	if !reflect.DeepEqual(got.Kills, []promclient.OOMKill{kill}) {
		t.Errorf("kills = %v, want %v", got.Kills, kill)
	}
	var instants []time.Time
	for _, sc := range seen {
		instants = append(instants, sc.now)
	}
	if want := []time.Time{t0, t0, t1, t1, t2, t2}; !reflect.DeepEqual(instants, want) {
		t.Errorf("stages ran as of %v, want each instant in turn", instants)
	}
	last := Compute(a.Request).Recs["app"]
	if float64(last.MemoryRequest.Value()) != c.Memory.Request[2].Value || float64(last.MemoryLimit.Value()) != c.Memory.Limit[2].Value {
		t.Errorf("last instant = %v / %v, want what Compute recommends as of it: %s / %s",
			c.Memory.Request[2].Value, c.Memory.Limit[2].Value, last.MemoryRequest, last.MemoryLimit)
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

	mem := got.Containers["app"].Memory
	if len(mem.Usage) != 1 || mem.Request != nil || mem.Limit != nil || len(got.Containers["app"].CPU.Request) != 1 {
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
