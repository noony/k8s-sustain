package prometheus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// ContainerValues maps container name → metric value (cores for CPU, bytes for memory).
type ContainerValues map[string]float64

// Client wraps the Prometheus HTTP API for k8s-sustain queries.
type Client struct {
	api     prometheusv1.API
	breaker *breaker
}

// Default circuit-breaker tuning: trip after 5 consecutive failures,
// stay open for 30 seconds. These values match queryTimeout so that one
// stuck reconcile (≈ 5 queries × queryTimeout) is enough to open it.
const (
	defaultBreakerMaxFailures = 5
	defaultBreakerCooldown    = 30 * time.Second
)

// New creates a Prometheus client targeting addr (e.g. "http://prometheus:9090").
func New(addr string) (*Client, error) {
	c, err := api.NewClient(api.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &Client{
		api:     prometheusv1.NewAPI(c),
		breaker: newBreaker(defaultBreakerMaxFailures, defaultBreakerCooldown),
	}, nil
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

// QueryWorkloadCPUByContainer returns the total CPU (cores) per container summed
// across all replicas of the workload, at the given quantile over the window.
// Reads the k8s_sustain:workload_cpu_usage:cores recording rule.
func (c *Client) QueryWorkloadCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`quantile_over_time(%.2f, k8s_sustain:workload_cpu_usage:cores{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m])`,
		quantile, namespace, ownerKind, ownerName, window,
	)
	return c.queryByContainer(ctx, expr)
}

// QueryWorkloadMemoryByContainer returns the total memory (bytes) per container summed
// across all replicas of the workload, at the given quantile over the window.
// Reads the k8s_sustain:workload_memory_usage:bytes recording rule.
func (c *Client) QueryWorkloadMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := fmt.Sprintf(
		`quantile_over_time(%.2f, k8s_sustain:workload_memory_usage:bytes{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m])`,
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

// minHistorySamples is the minimum count of rate5m samples required in the
// recommendation window before we'll emit a recommendation. Containers younger
// than this produce noisy/zero rates that would otherwise floor the recommendation
// and trigger an immediate recycle on the next reconcile.
const minHistorySamples = 12

// HasSufficientHistory reports whether the workload has enough rate5m samples
// over the window to make a meaningful recommendation. Probes
// k8s_sustain:container_cpu_usage_by_workload:rate5m via count_over_time.
func (c *Client) HasSufficientHistory(ctx context.Context, namespace, ownerKind, ownerName, window string) (bool, error) {
	if !c.breaker.allow() {
		return false, ErrCircuitOpen
	}
	expr := fmt.Sprintf(
		`max(count_over_time(k8s_sustain:container_cpu_usage_by_workload:rate5m{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m]))`,
		namespace, ownerKind, ownerName, window,
	)
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	result, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		c.breaker.failure()
		return false, fmt.Errorf("prometheus history probe %q: %w", expr, err)
	}
	c.breaker.success()
	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return false, nil
	}
	return float64(vector[0].Value) >= float64(minHistorySamples), nil
}

// OOMSignal carries the OOM context for a single workload over the past 24h.
type OOMSignal struct {
	OOMCount        float64
	PeakMemoryBytes ContainerValues
}

// QueryWorkloadOOMSignal returns the recent OOM count (24h) and the peak
// per-container memory working-set bytes observed alongside it. Used as a floor
// signal: if a workload OOM'd, never recommend memory below max(peak, current).
func (c *Client) QueryWorkloadOOMSignal(ctx context.Context, namespace, ownerKind, ownerName string) (OOMSignal, error) {
	if !c.breaker.allow() {
		return OOMSignal{}, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	oomExpr := fmt.Sprintf(
		`sum(k8s_sustain:workload_oom_24h{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	oomRes, _, err := c.api.Query(ctx, oomExpr, time.Now())
	if err != nil {
		c.breaker.failure()
		return OOMSignal{}, fmt.Errorf("prometheus oom probe %q: %w", oomExpr, err)
	}
	c.breaker.success()
	var oomCount float64
	if vec, ok := oomRes.(model.Vector); ok && len(vec) > 0 {
		oomCount = float64(vec[0].Value)
	}

	// Use the dedicated peak rule (kernel high-water + OOM-scoped limit fallback).
	// Working-set sampled at scrape interval misses sub-second spikes that
	// trigger the kill — `container_memory_max_usage_bytes` (cgroup v1) and
	// `container_memory_peak_working_set_bytes` (cgroup v2) survive across scrape gaps.
	peakExpr := fmt.Sprintf(
		`max by (container) (k8s_sustain:container_peak_memory_24h:bytes{namespace=%q,owner_kind=%q,owner_name=%q})`,
		namespace, ownerKind, ownerName,
	)
	if !c.breaker.allow() {
		return OOMSignal{OOMCount: oomCount}, nil
	}
	peakRes, _, err := c.api.Query(ctx, peakExpr, time.Now())
	if err != nil {
		c.breaker.failure()
		return OOMSignal{OOMCount: oomCount}, fmt.Errorf("prometheus peak probe %q: %w", peakExpr, err)
	}
	c.breaker.success()
	peaks := ContainerValues{}
	if vec, ok := peakRes.(model.Vector); ok {
		for _, s := range vec {
			name := string(s.Metric["container"])
			if name != "" {
				peaks[name] = float64(s.Value)
			}
		}
	}
	return OOMSignal{OOMCount: oomCount, PeakMemoryBytes: peaks}, nil
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

	if !c.breaker.allow() {
		// Non-fatal: skip OOM lookup while breaker is open.
		return nil, nil
	}
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
		c.breaker.failure()
		return nil, nil //nolint:nilerr
	}
	c.breaker.success()

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, nil
	}

	// Dedup consecutive positive samples per (pod, container). `increase()` over
	// a counter that keeps growing during CrashLoopBackOff produces a positive
	// sample at every step until the loop ends, which would otherwise spam the
	// chart with one marker per step. Collapse anything within a `2 × step`
	// gap (floor 30s) to a single event.
	dedupGap := 2 * time.Duration(stepDur)
	if dedupGap < 30*time.Second {
		dedupGap = 30 * time.Second
	}
	var events []OOMEvent
	for _, stream := range matrix {
		container := string(stream.Metric["container"])
		pod := string(stream.Metric["pod"])
		if container == "" {
			continue
		}
		var lastTs time.Time
		for _, v := range stream.Values {
			if float64(v.Value) <= 0 {
				continue
			}
			ts := v.Timestamp.Time()
			if !lastTs.IsZero() && ts.Sub(lastTs) < dedupGap {
				continue
			}
			lastTs = ts
			events = append(events, OOMEvent{
				Timestamp: ts,
				Container: container,
				Pod:       pod,
			})
		}
	}
	return events, nil
}

func (c *Client) queryRangeByContainer(ctx context.Context, expr, window, step string) (ContainerTimeSeries, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
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
		c.breaker.failure()
		return nil, fmt.Errorf("prometheus range query %q: %w", expr, err)
	}
	c.breaker.success()

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
	if !c.breaker.allow() {
		return ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := c.api.Query(ctx, "up", time.Now())
	if err != nil {
		c.breaker.failure()
		return fmt.Errorf("prometheus unreachable: %w", err)
	}
	c.breaker.success()
	return nil
}

func (c *Client) queryByContainer(ctx context.Context, expr string) (ContainerValues, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	result, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("prometheus query %q: %w", expr, err)
	}
	c.breaker.success()

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

// QueryReplicaCountMedian returns the median replica count of the workload over
// the window. Returns 0 with no error if the rule produced no samples.
// Reads the k8s_sustain:workload_replicas:count recording rule.
func (c *Client) QueryReplicaCountMedian(ctx context.Context, namespace, ownerKind, ownerName, window string) (float64, error) {
	if !c.breaker.allow() {
		return 0, ErrCircuitOpen
	}
	expr := fmt.Sprintf(
		`quantile_over_time(0.50, k8s_sustain:workload_replicas:count{namespace=%q,owner_kind=%q,owner_name=%q}[%s:1m])`,
		namespace, ownerKind, ownerName, window,
	)

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	result, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		c.breaker.failure()
		return 0, fmt.Errorf("prometheus query %q: %w", expr, err)
	}
	c.breaker.success()
	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, nil
	}
	return float64(vector[0].Value), nil
}

// dashboardQueryTimeout bounds dashboard-side reads of recording rules.
const dashboardQueryTimeout = 10 * time.Second

// QueryInstant runs a single instant query and returns the scalar/first-vector
// value. Returns 0 with no error if the query produces no samples.
func (c *Client) QueryInstant(ctx context.Context, expr string) (float64, error) {
	if !c.breaker.allow() {
		return 0, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardQueryTimeout)
	defer cancel()
	v, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		c.breaker.failure()
		return 0, fmt.Errorf("instant query %q: %w", expr, err)
	}
	c.breaker.success()
	switch typed := v.(type) {
	case model.Vector:
		if len(typed) == 0 {
			return 0, nil
		}
		return float64(typed[0].Value), nil
	case *model.Scalar:
		return float64(typed.Value), nil
	default:
		return 0, nil
	}
}

// QueryRange runs a range query for a single series and returns its time-stamped
// values. If the query produces multiple series, only the first is returned.
func (c *Client) QueryRange(ctx context.Context, expr, window, step string) ([]TimeValue, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardQueryTimeout)
	defer cancel()
	end := time.Now()
	dur, err := model.ParseDuration(window)
	if err != nil {
		return nil, fmt.Errorf("parse window %q: %w", window, err)
	}
	stp, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parse step %q: %w", step, err)
	}
	r := prometheusv1.Range{Start: end.Add(-time.Duration(dur)), End: end, Step: time.Duration(stp)}
	v, _, err := c.api.QueryRange(ctx, expr, r)
	if err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("range query %q: %w", expr, err)
	}
	c.breaker.success()
	matrix, ok := v.(model.Matrix)
	if !ok || len(matrix) == 0 {
		return nil, nil
	}
	out := make([]TimeValue, 0, len(matrix[0].Values))
	for _, p := range matrix[0].Values {
		out = append(out, TimeValue{Timestamp: p.Timestamp.Time(), Value: float64(p.Value)})
	}
	return out, nil
}

// QueryByLabel runs an instant query and returns a map of label-value -> sample value.
// Used for per-policy and per-workload aggregates.
func (c *Client) QueryByLabel(ctx context.Context, expr, label string) (map[string]float64, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardQueryTimeout)
	defer cancel()
	v, _, err := c.api.Query(ctx, expr, time.Now())
	if err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("by-label query %q: %w", expr, err)
	}
	c.breaker.success()
	vec, ok := v.(model.Vector)
	if !ok {
		return map[string]float64{}, nil
	}
	out := map[string]float64{}
	for _, sample := range vec {
		key := string(sample.Metric[model.LabelName(label)])
		if key == "" {
			continue
		}
		out[key] = float64(sample.Value)
	}
	return out, nil
}

// QueryByLabels runs an instant query and returns a map keyed by the named
// labels joined with '|'. Samples missing any of the requested labels are
// skipped. Useful when several labels jointly identify a series.
func (c *Client) QueryByLabels(ctx context.Context, query string, labels ...string) (map[string]float64, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardQueryTimeout)
	defer cancel()
	v, _, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		c.breaker.failure()
		return nil, fmt.Errorf("by-labels query %q: %w", query, err)
	}
	c.breaker.success()
	vec, ok := v.(model.Vector)
	if !ok {
		return map[string]float64{}, nil
	}
	out := make(map[string]float64, len(vec))
	for _, s := range vec {
		parts := make([]string, 0, len(labels))
		complete := true
		for _, l := range labels {
			lv, ok := s.Metric[model.LabelName(l)]
			if !ok {
				complete = false
				break
			}
			parts = append(parts, string(lv))
		}
		if !complete {
			continue
		}
		out[strings.Join(parts, "|")] = float64(s.Value)
	}
	return out, nil
}
