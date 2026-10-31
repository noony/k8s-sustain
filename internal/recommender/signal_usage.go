package recommender

import (
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

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

// usageOf is the usage signal of res, false for a resource none reads.
func usageOf(res resourceKind) (usage, bool) {
	switch res {
	case cpuResource:
		return cpuUsage, true
	case memoryResource:
		return memoryUsage, true
	}
	return usage{}, false
}

// WithCPUUsage sets each container's CPU usage percentile, in cores.
func WithCPUUsage(cores promclient.ContainerValues) InputsOption {
	return setting(cpuUsage, usageValues(maps.Clone(cores)))
}

// WithMemoryUsage sets each container's memory usage percentile, in bytes.
func WithMemoryUsage(bytes promclient.ContainerValues) InputsOption {
	return setting(memoryUsage, usageValues(maps.Clone(bytes)))
}

// Resolved reports whether the inputs hold the usage of any container, CPU or
// memory: an identity Prometheus has no usage sample of is not resolved, which
// is no failure.
func (in *WorkloadInputs) Resolved() bool {
	return len(valuesOf[usageValues](in, cpuUsage)) > 0 || len(valuesOf[usageValues](in, memoryUsage)) > 0
}

// Usage is the usage percentile of res observed for the container named
// container, in cores or bytes: zero when none was.
func (in *WorkloadInputs) Usage(container string, res corev1.ResourceName) float64 {
	v, _ := in.usage(container, resourceKind(res))
	return v
}

// usage is the usage percentile of res observed for the container named
// container, false when none was.
func (in *WorkloadInputs) usage(container string, res resourceKind) (float64, bool) {
	u, ok := usageOf(res)
	if !ok {
		return 0, false
	}
	v, ok := valuesOf[usageValues](in, u)[container]
	return v, ok
}

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

// usageValues are a usage signal's values: each container's usage percentile,
// in cores or bytes.
type usageValues map[string]float64

func (v usageValues) observed() []string { return slices.Collect(maps.Keys(v)) }

func (usageValues) excusesAge() bool { return false }

// collect keeps the last sample of each container. That is safe only because
// the contract holds the rule to aggregating by exactly shardLabels, so at
// most one series per container of an identity can be returned.
func (u usage) collect(samples []promclient.ShardSample) signalValues {
	values := make(usageValues, len(samples))
	for _, s := range samples {
		values[s.Container] = s.Value
	}
	return values
}

func (u usage) slot() slot { return slot{role: base, resource: u.resource} }

func (u usage) contribute(sc stageContext) (contribution, bool) {
	v, ok := valuesOf[usageValues](sc.inputs, u)[sc.container.Name]
	return contribution{value: v, anchors: true}, ok
}

// stage records the percentile at full precision: nanocores or bytes.
func (u usage) stage(_ stageContext, value float64, _ bool) sustainv1alpha1.TraceStage {
	return sustainv1alpha1.TraceStage{Stage: stagePercentile, Value: u.resource.quantity(value)}
}
