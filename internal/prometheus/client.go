package prometheus

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// ContainerValues maps container name → metric value (cores for CPU, bytes for memory).
type ContainerValues map[string]float64

// Client wraps the Prometheus HTTP API for k8s-sustain queries.
type Client struct {
	api prometheusv1.API
}

// New creates a Prometheus client targeting addr (e.g. "http://prometheus:9090").
func New(addr string) (*Client, error) {
	c, err := api.NewClient(api.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &Client{api: prometheusv1.NewAPI(c)}, nil
}

// QueryCPUByContainer returns per-container CPU usage (cores) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_cpu_usage_by_workload:rate5m recording rule.
func (c *Client) QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_cpu_usage_by_workload:rate5m{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, window,
	)
	return c.queryByContainer(ctx, expr)
}

// QueryMemoryByContainer returns per-container memory working set (bytes) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_memory_by_workload:bytes recording rule.
func (c *Client) QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_memory_by_workload:bytes{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, window,
	)
	return c.queryByContainer(ctx, expr)
}

// TimeSeries holds a single time-series: metric labels plus timestamped values.
type TimeSeries struct {
	Labels map[string]string `json:"labels"`
	Values []TimeValue       `json:"values"`
}

// TimeValue is a single (timestamp, value) data point.
type TimeValue struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// ContainerTimeSeries maps container name → time-series data points.
type ContainerTimeSeries map[string][]TimeValue

// QueryCPURangeByContainer returns per-container CPU usage time-series (cores)
// over the specified window with the given step resolution.
func (c *Client) QueryCPURangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`avg by (container) (k8s_sustain:container_cpu_usage_by_workload:rate5m{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	return c.queryRangeByContainer(ctx, expr, window, step)
}

// QueryMemoryRangeByContainer returns per-container memory working set time-series (bytes)
// over the specified window with the given step resolution.
func (c *Client) QueryMemoryRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`avg by (container) (k8s_sustain:container_memory_by_workload:bytes{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	return c.queryRangeByContainer(ctx, expr, window, step)
}

// QueryCPURequestRangeByContainer returns per-container CPU request time-series (cores)
// over the specified window with the given step resolution.
// Uses the k8s_sustain:container_cpu_requests_by_workload:cores recording rule.
func (c *Client) QueryCPURequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`max by (container) (k8s_sustain:container_cpu_requests_by_workload:cores{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	return c.queryRangeByContainer(ctx, expr, window, step)
}

// QueryMemoryRequestRangeByContainer returns per-container memory request time-series (bytes)
// over the specified window with the given step resolution.
// Uses the k8s_sustain:container_memory_requests_by_workload:bytes recording rule.
func (c *Client) QueryMemoryRequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName, window, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`max by (container) (k8s_sustain:container_memory_requests_by_workload:bytes{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	return c.queryRangeByContainer(ctx, expr, window, step)
}

// QueryCPURecommendationRangeByContainer returns per-container sliding-window CPU recommendation
// time-series (cores) — at each step, the quantile is computed over the trailing window.
func (c *Client) QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_cpu_usage_by_workload:rate5m{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, recWindow,
	)
	return c.queryRangeByContainer(ctx, expr, timeRange, step)
}

// QueryMemoryRecommendationRangeByContainer returns per-container sliding-window memory recommendation
// time-series (bytes) — at each step, the quantile is computed over the trailing window.
func (c *Client) QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow, timeRange, step string) (ContainerTimeSeries, error) {
	expr := fmt.Sprintf(
		`avg by (container) (quantile_over_time(%.2f, k8s_sustain:container_memory_by_workload:bytes{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		quantile, namespace, ownerKind, ownerName, recWindow,
	)
	return c.queryRangeByContainer(ctx, expr, timeRange, step)
}

// OOMEvent represents a single OOM kill event for a container.
type OOMEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Container string    `json:"container"`
	Pod       string    `json:"pod"`
}

// QueryOOMKillEvents returns OOM kill events for a workload over the specified window.
// Uses kube_pod_container_status_restarts_total joined with
// kube_pod_container_status_last_terminated_reason{reason="OOMKilled"} to detect
// restart events caused by OOM kills.
func (c *Client) QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName, window, step string) ([]OOMEvent, error) {
	// Detect restarts where the last termination reason was OOMKilled,
	// scoped to the workload via the pod_workload mapping.
	expr := fmt.Sprintf(
		`increase(kube_pod_container_status_restarts_total{namespace=%q, container!="", container!="POD"}[%s])
		 * on(namespace, pod, container) group_left()
		   kube_pod_container_status_last_terminated_reason{namespace=%q, reason="OOMKilled", container!="", container!="POD"}
		 * on(namespace, pod) group_left(owner_kind, owner_name)
		   k8s_sustain:pod_workload{namespace=%q, owner_kind=%q, owner_name=%q}`,
		namespace, step,
		namespace,
		namespace, ownerKind, ownerName,
	)

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	windowDur, err := model.ParseDuration(window)
	if err != nil {
		return nil, fmt.Errorf("parsing window %q: %w", window, err)
	}
	stepDur, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parsing step %q: %w", step, err)
	}

	end := time.Now()
	start := end.Add(-time.Duration(windowDur))

	result, _, err := c.api.QueryRange(ctx, expr, prometheusv1.Range{
		Start: start,
		End:   end,
		Step:  time.Duration(stepDur),
	})
	if err != nil {
		// Non-fatal: OOM data may not be available (missing kube-state-metrics etc.)
		return nil, nil //nolint:nilerr
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, nil
	}

	var events []OOMEvent
	for _, stream := range matrix {
		container := string(stream.Metric["container"])
		pod := string(stream.Metric["pod"])
		if container == "" {
			continue
		}
		for _, v := range stream.Values {
			if float64(v.Value) > 0 {
				events = append(events, OOMEvent{
					Timestamp: v.Timestamp.Time(),
					Container: container,
					Pod:       pod,
				})
			}
		}
	}
	return events, nil
}

func (c *Client) queryRangeByContainer(ctx context.Context, expr, window, step string) (ContainerTimeSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	windowDur, err := model.ParseDuration(window)
	if err != nil {
		return nil, fmt.Errorf("parsing window %q: %w", window, err)
	}
	stepDur, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parsing step %q: %w", step, err)
	}

	end := time.Now()
	start := end.Add(-time.Duration(windowDur))

	result, _, err := c.api.QueryRange(ctx, expr, prometheusv1.Range{
		Start: start,
		End:   end,
		Step:  time.Duration(stepDur),
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus range query %q: %w", expr, err)
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for range query", result)
	}

	series := make(ContainerTimeSeries, len(matrix))
	for _, stream := range matrix {
		name := string(stream.Metric["container"])
		if name == "" {
			continue
		}
		values := make([]TimeValue, 0, len(stream.Values))
		for _, v := range stream.Values {
			values = append(values, TimeValue{
				Timestamp: v.Timestamp.Time(),
				Value:     float64(v.Value),
			})
		}
		series[name] = values
	}
	return series, nil
}

// queryTimeout is the maximum duration for a single Prometheus query.
const queryTimeout = 30 * time.Second

// Ping checks that the Prometheus server is reachable by executing a trivial query.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := c.api.Query(ctx, "up", time.Now())
	if err != nil {
		return fmt.Errorf("prometheus unreachable: %w", err)
	}
	return nil
}

func (c *Client) queryByContainer(ctx context.Context, expr string) (ContainerValues, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	result, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", expr, err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T", result)
	}

	values := make(ContainerValues, len(vector))
	for _, sample := range vector {
		name := string(sample.Metric["container"])
		if name != "" {
			values[name] = float64(sample.Value)
		}
	}
	return values, nil
}
