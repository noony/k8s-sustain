package recommender

import (
	"maps"
	"testing"

	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

var dottedShard = promclient.Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"payments.worker", "api"}}

// Each usage signal reads its own resource's percentile and window, defaulting
// to p95 over 7 days, from its own rule, and selects the shard's owner names
// escaped: names are RFC 1123 subdomains and may contain '.'.
func TestUsage_QueryReadsItsResourcesPercentileAndWindow(t *testing.T) {
	cfg := sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{Window: "24h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](90)}},
		Memory: sustainv1alpha1.ResourceConfig{Window: "1h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](99)}},
	}
	const sel = `{namespace="prod",owner_kind="Deployment",owner_name=~"payments\\.worker|api"}`
	cases := []struct {
		name   string
		signal usage
		cfg    sustainv1alpha1.ResourcesConfigs
		want   string
	}{
		{"cpu", cpuUsage, cfg, `quantile_over_time(0.90, k8s_sustain:workload_max_pod_cpu:cores` + sel + `[24h])`},
		{"memory", memoryUsage, cfg, `quantile_over_time(0.99, k8s_sustain:workload_max_pod_memory:bytes` + sel + `[1h])`},
		{"cpu defaults", cpuUsage, sustainv1alpha1.ResourcesConfigs{}, `quantile_over_time(0.95, k8s_sustain:workload_max_pod_cpu:cores` + sel + `[168h])`},
		{"memory defaults", memoryUsage, sustainv1alpha1.ResourcesConfigs{}, `quantile_over_time(0.95, k8s_sustain:workload_max_pod_memory:bytes` + sel + `[168h])`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.signal.query(tc.cfg, dottedShard); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A windowed read loads one sample per minute of the window, so a container
// costs its resource's window in minutes, whatever the other resource's is.
func TestUsage_ShardCostIsItsWindowInMinutes(t *testing.T) {
	cfg := sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Window: "1h"}}

	if got := cpuUsage.samplesPerContainer(cfg); got != 60 {
		t.Errorf("cpu over 1h costs %d, want 60", got)
	}
	if got := memoryUsage.samplesPerContainer(cfg); got != 7*24*60 {
		t.Errorf("memory over the default window costs %d, want %d", got, 7*24*60)
	}
}

// Usage is what a request is computed from: an identity whose usage cannot be
// read has no inputs.
func TestUsage_IsRequired(t *testing.T) {
	for _, s := range []usage{cpuUsage, memoryUsage} {
		if !s.required() {
			t.Errorf("%s usage is best-effort, want required", s.name())
		}
	}
}

// Each usage signal keeps one value per container in its own field, leaves
// the other alone, and holds empty values when the identity had no samples.
func TestUsage_CollectsOneValuePerContainer(t *testing.T) {
	samples := []promclient.ShardSample{
		{Container: "app", Value: 0.5},
		{Container: "sidecar", Value: 0.05},
	}
	in := &WorkloadInputs{MemPerPod: promclient.ContainerValues{"app": 64}}

	cpuUsage.collect(in, samples)

	if want := (promclient.ContainerValues{"app": 0.5, "sidecar": 0.05}); !maps.Equal(in.CPUPerPod, want) {
		t.Errorf("cpu = %v, want %v", in.CPUPerPod, want)
	}
	if in.MemPerPod["app"] != 64 {
		t.Errorf("memory = %v, want it untouched", in.MemPerPod)
	}

	memoryUsage.collect(in, nil)

	if in.MemPerPod == nil || len(in.MemPerPod) != 0 {
		t.Errorf("memory = %v, want empty and non-nil after no samples", in.MemPerPod)
	}
}

// A usage signal contributes the percentile of a container it has samples for,
// which justifies a request on its own, and records it as the trace's
// percentile at its resource's precision.
func TestUsage_StageAndTrace(t *testing.T) {
	in := &WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1234},
		MemPerPod: promclient.ContainerValues{"app": 100.7 * mib},
	}

	for _, tc := range []struct {
		signal usage
		want   string
	}{
		{cpuUsage, "123400u"},
		{memoryUsage, "105591603"},
	} {
		t.Run(tc.signal.name(), func(t *testing.T) {
			c, ok := tc.signal.contribute(in, "app")
			if !ok || !c.anchors {
				t.Fatalf("contribution = %+v, %v, want an anchoring value", c, ok)
			}
			if _, ok := tc.signal.contribute(in, "absent"); ok {
				t.Error("a container without samples contributed")
			}
			var tr sustainv1alpha1.ResourceTrace
			tc.signal.record(&tr, c.value, true)
			assertQty(t, "percentile", tr.Percentile, tc.want)
			if tr.OOMFloor != nil {
				t.Errorf("usage recorded an OOM floor: %+v", tr.OOMFloor)
			}
		})
	}
}
