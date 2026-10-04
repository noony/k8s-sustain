package recommender

import (
	"maps"
	"slices"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// usage is a base signal: one resource's usage percentile on the busiest
// replica, over the window the Policy sets for that resource. Because the
// percentile already covers the hottest replica, there is no replica division
// and no separate per-pod floor.
type usage struct {
	resource resourceKind
	// rule holds, per identity and container, the busiest pod's usage at each
	// minute.
	rule string
}

var (
	cpuUsage    = usage{resource: cpuResource, rule: promclient.MetricWorkloadMaxPodCPUCores}
	memoryUsage = usage{resource: memoryResource, rule: promclient.MetricWorkloadMaxPodMemoryBytes}
)

func (u usage) name() string { return string(u.resource) }

// required: usage is what a request is computed from.
func (u usage) required() bool { return true }

func (u usage) window(cfg sustainv1alpha1.ResourcesConfigs) string {
	return ResourceWindow(u.resource.config(cfg).Window)
}

// samplesPerContainer is one sample per minute of the window, the rule's
// evaluation interval.
func (u usage) samplesPerContainer(cfg sustainv1alpha1.ResourcesConfigs) int {
	return promclient.WindowMinutes(u.window(cfg))
}

func (u usage) query(cfg sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string {
	quantile := PercentileQuantile(u.resource.config(cfg).Requests.Percentile)
	return promclient.QuantileOverTime(quantile, u.rule, shard.Selector(), u.window(cfg))
}

// collect keeps the last sample of each container. That is safe only because
// the rule already aggregates by (namespace, owner_kind, owner_name,
// container) (see charts/k8s-sustain/values.yaml), so at most one series per
// container of an identity can be returned. If its by() clause ever drops one
// of those labels, this breaks silently.
func (u usage) collect(in *WorkloadInputs, samples []promclient.ShardSample) {
	values := make(promclient.ContainerValues, len(samples))
	for _, s := range samples {
		values[s.Container] = s.Value
	}
	*u.values(in) = values
}

func (u usage) observed(in *WorkloadInputs) []string {
	return slices.Collect(maps.Keys(*u.values(in)))
}

func (u usage) slot() slot { return slot{role: base, resource: u.resource} }

func (u usage) contribute(in *WorkloadInputs, container string) (contribution, bool) {
	v, ok := (*u.values(in))[container]
	return contribution{value: v, anchors: true}, ok
}

func (u usage) record(t *sustainv1alpha1.ResourceTrace, value float64, _ bool) {
	t.Percentile = u.resource.quantity(value)
}

func (u usage) values(in *WorkloadInputs) *promclient.ContainerValues {
	if u.resource == cpuResource {
		return &in.CPUPerPod
	}
	return &in.MemPerPod
}
