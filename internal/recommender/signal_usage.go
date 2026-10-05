package recommender

import (
	"maps"
	"slices"
	"time"

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
	// scraped is the cAdvisor metric rule computes usage from.
	scraped string
}

const stagePercentile = "percentile"

var (
	cpuUsage = usage{
		resource: cpuResource,
		rule:     promclient.MetricWorkloadMaxPodCPUCores,
		scraped:  "container_cpu_usage_seconds_total",
	}
	memoryUsage = usage{
		resource: memoryResource,
		rule:     promclient.MetricWorkloadMaxPodMemoryBytes,
		scraped:  "container_memory_working_set_bytes",
	}
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

// contract: the rule is read as a plain range vector, so its evaluation
// interval is the percentile's sample resolution and what samplesPerContainer
// counts.
func (u usage) contract() PromContract {
	return PromContract{
		Rules:            []string{u.rule},
		Labels:           shardLabels,
		Interval:         time.Minute,
		CAdvisor:         []string{u.scraped},
		KubeStateMetrics: podWorkloadMetrics,
	}
}

// collect keeps the last sample of each container. That is safe only because
// the contract holds the rule to aggregating by exactly shardLabels, so at
// most one series per container of an identity can be returned.
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

func (u usage) excusesAge(*WorkloadInputs) bool { return false }

func (u usage) slot() slot { return slot{role: base, resource: u.resource} }

func (u usage) contribute(in *WorkloadInputs, container string) (contribution, bool) {
	v, ok := (*u.values(in))[container]
	return contribution{value: v, anchors: true}, ok
}

// stage records the percentile at full precision: nanocores or bytes.
func (u usage) stage(_ *WorkloadInputs, _ string, value float64, _ bool) sustainv1alpha1.TraceStage {
	return sustainv1alpha1.TraceStage{Stage: stagePercentile, Value: u.resource.quantity(value)}
}

func (u usage) values(in *WorkloadInputs) *promclient.ContainerValues {
	if u.resource == cpuResource {
		return &in.CPUPerPod
	}
	return &in.MemPerPod
}
