package prometheus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// logWarnings logs Prometheus query warnings so partial results are visible.
func logWarnings(ctx context.Context, expr string, warnings prometheusv1.Warnings) {
	if len(warnings) == 0 {
		return
	}
	log.FromContext(ctx).Info("prometheus query returned warnings", "expr", expr, "warnings", warnings)
}

// ContainerValues maps container name to metric value (cores or bytes).
type ContainerValues map[string]float64

func workloadSelector(namespace, ownerKind, ownerName string) string {
	return fmt.Sprintf("{namespace=%q,owner_kind=%q,owner_name=%q}", namespace, ownerKind, ownerName)
}

// quantileOverTimeExpr reads the rule as a plain range vector, not a
// `[window:1m]` subquery: the rules are already materialised at 1m, so a
// subquery adds cost, not fidelity.
func quantileOverTimeExpr(quantile float64, rule, selector, window string) string {
	return fmt.Sprintf("quantile_over_time(%.2f, %s%s[%s])", quantile, rule, selector, window)
}

func avgByContainer(inner string) string {
	return fmt.Sprintf("avg by (container) (%s)", inner)
}

func maxByContainer(inner string) string {
	return fmt.Sprintf("max by (container) (%s)", inner)
}

// recordQueryFailure counts a failed query against the breaker. ctx must be
// the per-call WithTimeout child. A collateral abort from an errgroup sibling
// (a non-context cause) is not counted; the sibling already counted its own.
func (c *Client) recordQueryFailure(ctx context.Context) {
	if ctx.Err() == nil {
		c.breaker.failure()
		return
	}
	if cause := context.Cause(ctx); errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		c.breaker.failure()
	}
}

// execInstant runs an instant query through the breaker, in-flight semaphore
// and per-call timeout. The semaphore is acquired before the timeout starts so
// queue time is never charged against the query budget.
func (c *Client) execInstant(ctx context.Context, expr string, ts time.Time, timeout time.Duration) (model.Value, error) {
	allowed, probe := c.breaker.allow()
	if !allowed {
		return nil, ErrCircuitOpen
	}
	release, err := c.acquire(ctx, probe)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, warnings, err := c.api.Query(ctx, expr, ts)
	if err != nil {
		c.recordQueryFailure(ctx)
		return nil, err
	}
	c.breaker.success()
	logWarnings(ctx, expr, warnings)
	return v, nil
}

// runRange is execInstant's range-query counterpart, minus the breaker.allow()
// gate, which callers run themselves. holdsProbe is allow()'s second return
// value; acquire needs it so it does not reject the half-open probe holder.
func (c *Client) runRange(ctx context.Context, expr string, r prometheusv1.Range, timeout time.Duration, holdsProbe bool) (model.Value, error) {
	release, err := c.acquire(ctx, holdsProbe)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, warnings, err := c.api.QueryRange(ctx, expr, r)
	if err != nil {
		c.recordQueryFailure(ctx)
		return nil, err
	}
	c.breaker.success()
	logWarnings(ctx, expr, warnings)
	return v, nil
}

func vectorToContainerValues(vec model.Vector) ContainerValues {
	values := make(ContainerValues, len(vec))
	for _, sample := range vec {
		name := string(sample.Metric["container"])
		if name != "" {
			values[name] = float64(sample.Value)
		}
	}
	return values
}

// Client wraps the Prometheus HTTP API for k8s-sustain queries.
type Client struct {
	api          prometheusv1.API
	breaker      *breaker
	queryTimeout time.Duration
	queueTimeout time.Duration
	// sem bounds concurrent in-flight queries. nil disables the bound.
	sem chan struct{}
	// transportSet records that WithTransportConfig was used, even with a zero
	// config, so it still conflicts with WithRoundTripper.
	transport    TransportConfig
	transportSet bool
	roundTripper http.RoundTripper
}

const (
	defaultBreakerMaxFailures = 5
	defaultBreakerCooldown    = 30 * time.Second
)

// Option configures a Client at construction time.
type Option func(*Client)

// WithQueryTimeout overrides the default per-query timeout.
func WithQueryTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.queryTimeout = d
		}
	}
}

// WithMaxInflight bounds concurrent in-flight queries; n <= 0 leaves the client
// unbounded. Prometheus's --query.max-concurrency defaults to 20 and is shared
// with every other consumer, so the default 8 stays well under it.
func WithMaxInflight(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.sem = make(chan struct{}, n)
		}
	}
}

// WithQueueTimeout overrides how long a query may wait for an in-flight slot.
func WithQueueTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.queueTimeout = d
		}
	}
}

// ErrInflightQueueAborted reports that a query never reached Prometheus because
// its context ended while queued behind the in-flight semaphore. It is kept
// distinct from a real query error and never counts against the breaker.
var ErrInflightQueueAborted = errors.New("prometheus: aborted while queued for an in-flight slot")

// acquire takes a slot from the in-flight semaphore and returns the release
// func. holdsProbe exempts the breaker's half-open probe holder from the
// post-queue re-check.
func (c *Client) acquire(ctx context.Context, holdsProbe bool) (func(), error) {
	if c.sem == nil {
		return func() {}, nil
	}
	// The wait gets its own deadline: the reconcile context carries none, so
	// a saturated semaphore would otherwise park the goroutine forever.
	ctx, cancel := context.WithTimeout(ctx, c.queueTimeout)
	defer cancel()

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrInflightQueueAborted, ctx.Err())
	}

	// Re-check the breaker after queuing without consuming the half-open probe.
	// The probe holder is exempt: allow() advances openUntil when handing out
	// the probe, so isOpen is true for it, and rejecting it here would keep the
	// breaker open forever.
	if !holdsProbe && c.breaker.isOpen() {
		<-c.sem
		return nil, ErrCircuitOpen
	}
	return func() { <-c.sem }, nil
}

// New creates a Prometheus client targeting addr. Options are applied before
// the api.Client is built because they decide its RoundTripper.
func New(addr string, opts ...Option) (*Client, error) {
	cli := &Client{
		breaker:      newBreaker(defaultBreakerMaxFailures, defaultBreakerCooldown),
		queryTimeout: defaultQueryTimeout,
		queueTimeout: defaultQueueTimeout,
	}
	for _, opt := range opts {
		opt(cli)
	}
	rt, err := cli.resolveRoundTripper()
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	c, err := api.NewClient(api.Config{Address: addr, RoundTripper: rt})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	cli.api = prometheusv1.NewAPI(c)
	return cli, nil
}

// QueryCPUByContainer returns per-container CPU (cores) at the given quantile
// over window, averaged across the workload's pods.
func (c *Client) QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerCPUUsageByWorkloadRate1m, workloadSelector(namespace, ownerKind, ownerName), window))
	return c.queryByContainer(ctx, expr)
}

// QueryMemoryByContainer returns per-container memory working set (bytes) at
// the given quantile over window, averaged across the workload's pods.
func (c *Client) QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerMemoryByWorkloadBytes, workloadSelector(namespace, ownerKind, ownerName), window))
	return c.queryByContainer(ctx, expr)
}

// QueryWorkloadCPUByContainer returns the per-container CPU quantile (cores)
// of the busiest replica over window, from the workload_max_pod_cpu rule.
func (c *Client) QueryWorkloadCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := quantileOverTimeExpr(quantile, MetricWorkloadMaxPodCPUCores, workloadSelector(namespace, ownerKind, ownerName), window)
	return c.queryByContainer(ctx, expr)
}

// QueryWorkloadMemoryByContainer returns the per-container memory quantile
// (bytes) of the busiest replica over window, from the workload_max_pod_memory rule.
func (c *Client) QueryWorkloadMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := quantileOverTimeExpr(quantile, MetricWorkloadMaxPodMemoryBytes, workloadSelector(namespace, ownerKind, ownerName), window)
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

// ContainerTimeSeries maps container name to time-series data points.
type ContainerTimeSeries map[string][]TimeValue

// QueryCPURangeByContainer returns per-container CPU usage time-series (cores)
// over the specified range with the given step resolution.
func (c *Client) QueryCPURangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(MetricContainerCPUUsageByWorkloadRate1m + workloadSelector(namespace, ownerKind, ownerName))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryMemoryRangeByContainer returns per-container memory working set time-series (bytes)
// over the specified range with the given step resolution.
func (c *Client) QueryMemoryRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(MetricContainerMemoryByWorkloadBytes + workloadSelector(namespace, ownerKind, ownerName))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryCPURequestRangeByContainer returns per-container CPU request time-series (cores).
func (c *Client) QueryCPURequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerCPURequestsByWorkloadCores, namespace, ownerKind, ownerName, r, step)
}

// QueryMemoryRequestRangeByContainer returns per-container memory request time-series (bytes).
func (c *Client) QueryMemoryRequestRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerMemoryRequestsByWorkloadBytes, namespace, ownerKind, ownerName, r, step)
}

// QueryCPULimitRangeByContainer returns per-container CPU limit time-series
// (cores) from the per-pod cgroup limit, not the workload spec.
func (c *Client) QueryCPULimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerCPULimitsByWorkloadCores, namespace, ownerKind, ownerName, r, step)
}

// QueryMemoryLimitRangeByContainer returns per-container memory limit time-series (bytes).
func (c *Client) QueryMemoryLimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerMemoryLimitsByWorkloadBytes, namespace, ownerKind, ownerName, r, step)
}

func (c *Client) queryMaxByContainerForWorkload(ctx context.Context, ruleName, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := maxByContainer(ruleName + workloadSelector(namespace, ownerKind, ownerName))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryCPURecommendationRangeByContainer returns the sliding-window CPU
// recommendation (cores) per container: at each step, the quantile over the
// trailing recWindow.
func (c *Client) QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerCPUUsageByWorkloadRate1m, workloadSelector(namespace, ownerKind, ownerName), recWindow))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryMemoryRecommendationRangeByContainer returns the sliding-window memory
// recommendation (bytes) per container: at each step, the quantile over the
// trailing recWindow.
func (c *Client) QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerMemoryByWorkloadBytes, workloadSelector(namespace, ownerKind, ownerName), recWindow))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// OOMSignal carries the OOM context for a single workload over the past 24h.
type OOMSignal struct {
	// OOMCounts is the per-container OOM count over 24h, so only containers
	// that OOMed get a memory floor.
	OOMCounts       ContainerValues
	PeakMemoryBytes ContainerValues
	// OOMLimitBytes is the cgroup limit observed at OOM time, per container.
	// Used as the bump anchor because peak working set can miss sub-scrape spikes.
	OOMLimitBytes ContainerValues
}

// TotalOOMs sums the per-container OOM counts.
func (s OOMSignal) TotalOOMs() float64 {
	var total float64
	for _, v := range s.OOMCounts {
		total += v
	}
	return total
}

// oomMetricNames are the recording rules fetched together by one __name__ regex.
var oomMetricNames = []string{
	MetricWorkloadOOM24h,
	MetricContainerPeakMemory24hBytes,
	MetricContainerOOMLimit24hBytes,
}

// oomSignalSelector builds the combined __name__ regex plus identity selector.
// Rule names are interpolated unescaped, so they must stay RE2-literal.
func oomSignalSelector(namespace, ownerKind, ownerName string) string {
	return fmt.Sprintf("{__name__=~%q,namespace=%q,owner_kind=%q,owner_name=%q}",
		strings.Join(oomMetricNames, "|"), namespace, ownerKind, ownerName)
}

// foldOOMVector aggregates the OOM vector client-side: sum by container for
// counts, max by container for peak and OOM-time limit, collapsing duplicate
// series from multiple kube-state-metrics replicas.
func foldOOMVector(vec model.Vector) OOMSignal {
	sig := OOMSignal{
		OOMCounts:       ContainerValues{},
		PeakMemoryBytes: ContainerValues{},
		OOMLimitBytes:   ContainerValues{},
	}
	// The !ok check is load-bearing: a first sample of 0 must still create the
	// key, because ComputeContainerRec gates on key presence.
	maxInto := func(m ContainerValues, key string, v float64) {
		if cur, ok := m[key]; !ok || v > cur {
			m[key] = v
		}
	}
	for _, sample := range vec {
		container := string(sample.Metric["container"])
		if container == "" {
			continue
		}
		v := float64(sample.Value)
		switch string(sample.Metric[model.MetricNameLabel]) {
		case MetricWorkloadOOM24h:
			sig.OOMCounts[container] += v
		case MetricContainerPeakMemory24hBytes:
			maxInto(sig.PeakMemoryBytes, container, v)
		case MetricContainerOOMLimit24hBytes:
			maxInto(sig.OOMLimitBytes, container, v)
		}
	}
	return sig
}

// QueryWorkloadOOMSignal returns the 24h per-container OOM counts, peak memory
// and OOM-time limit for a workload, fetched in one query and folded client-side.
func (c *Client) QueryWorkloadOOMSignal(ctx context.Context, namespace, ownerKind, ownerName string) (OOMSignal, error) {
	expr := oomSignalSelector(namespace, ownerKind, ownerName)
	result, err := c.execInstant(ctx, expr, time.Now(), c.queryTimeout)
	if err != nil {
		return OOMSignal{}, wrapQueryErr("oom signal query", expr, err)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return OOMSignal{}, fmt.Errorf("unexpected prometheus result type %T for oom signal", result)
	}
	return foldOOMVector(vector), nil
}

// OOMEvent represents a single OOM kill event for a container.
type OOMEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Container string    `json:"container"`
	Pod       string    `json:"pod"`
}

// QueryOOMKillEvents returns OOM kill events for a workload over r, from
// restart increases gated on last_terminated_reason="OOMKilled".
func (c *Client) QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) ([]OOMEvent, error) {
	stepDur, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parsing step %q: %w", step, err)
	}

	// increase() needs at least two samples in its lookback; kube-state-metrics
	// scrapes every 30-60s, so floor the lookback at 5m regardless of step.
	lookback := max(time.Duration(stepDur), 5*time.Minute)
	lookbackExpr := model.Duration(lookback).String()

	// Both sides are aggregated with max by (namespace, pod, container) to drop
	// KSM scrape-target labels; otherwise a KSM rollout inside the window makes
	// the group_left join fail with many-to-many matching.
	expr := fmt.Sprintf(
		`max by (namespace, pod, container, owner_kind, owner_name) (
		   max by (namespace, pod, container) (
		     increase(kube_pod_container_status_restarts_total{namespace=%q, container!="", container!="POD"}[%s])
		   )
		   * on(namespace, pod, container) group_left()
		     max by (namespace, pod, container) (
		       kube_pod_container_status_last_terminated_reason{namespace=%q, reason="OOMKilled", container!="", container!="POD"} == 1
		     )
		   * on(namespace, pod) group_left(owner_kind, owner_name)
		     %s{namespace=%q, owner_kind=%q, owner_name=%q}
		 )`,
		namespace, lookbackExpr,
		namespace,
		MetricPodWorkload, namespace, ownerKind, ownerName,
	)

	allowed, probe := c.breaker.allow()
	if !allowed {
		// Non-fatal: skip OOM lookup while breaker is open.
		return nil, nil
	}

	result, err := c.runRange(ctx, expr, prometheusv1.Range{
		Start: r.Start,
		End:   r.End,
		Step:  time.Duration(stepDur),
	}, c.queryTimeout, probe)
	if err != nil {
		// Non-fatal: OOM data may be missing (no kube-state-metrics); keep the
		// dashboard rendering. runRange already recorded the breaker failure.
		return nil, nil //nolint:nilerr // intentional non-fatal swallow, see comment above
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, nil
	}

	// increase() smears each restart flat across the lookback; a second OOM
	// bumps the value by ~1. Emit on each upward step above 0.5 to absorb
	// extrapolation noise.
	const valueBumpThreshold = 0.5
	var events []OOMEvent
	for _, stream := range matrix {
		container := string(stream.Metric["container"])
		pod := string(stream.Metric["pod"])
		if container == "" {
			continue
		}
		var lastValue float64
		for _, v := range stream.Values {
			cur := float64(v.Value)
			if cur <= 0 {
				lastValue = 0
				continue
			}
			if cur > lastValue+valueBumpThreshold {
				events = append(events, OOMEvent{
					Timestamp: v.Timestamp.Time(),
					Container: container,
					Pod:       pod,
				})
			}
			lastValue = cur
		}
	}
	return events, nil
}

func (c *Client) queryRangeByContainer(ctx context.Context, expr string, r TimeRange, step string) (ContainerTimeSeries, error) {
	allowed, probe := c.breaker.allow()
	if !allowed {
		return nil, ErrCircuitOpen
	}

	stepDur, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parsing step %q: %w", step, err)
	}

	result, err := c.runRange(ctx, expr, prometheusv1.Range{
		Start: r.Start,
		End:   r.End,
		Step:  time.Duration(stepDur),
	}, c.queryTimeout, probe)
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

// defaultQueryTimeout is sized for background reconciles; the webhook overrides it.
const defaultQueryTimeout = 30 * time.Second

// defaultQueueTimeout bounds how long a query waits for an in-flight slot. It
// only guarantees termination, since the reconcile context has no deadline,
// and is kept separate from the query timeout: queue time is not query time.
const defaultQueueTimeout = 2 * time.Minute

// Ping checks that the Prometheus server is reachable by executing a trivial query.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.execInstant(ctx, "up", time.Now(), 5*time.Second); err != nil {
		if errors.Is(err, ErrCircuitOpen) {
			return err
		}
		return fmt.Errorf("prometheus unreachable: %w", err)
	}
	return nil
}

// wrapQueryErr passes ErrCircuitOpen through unchanged so callers can detect an
// open breaker; any other error is wrapped as `<prefix> "<expr>": <err>`.
func wrapQueryErr(prefix, expr string, err error) error {
	if errors.Is(err, ErrCircuitOpen) {
		return err
	}
	return fmt.Errorf("%s %q: %w", prefix, expr, err)
}

func (c *Client) queryByContainer(ctx context.Context, expr string) (ContainerValues, error) {
	result, err := c.execInstant(ctx, expr, time.Now(), c.queryTimeout)
	if err != nil {
		return nil, wrapQueryErr("prometheus query", expr, err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T", result)
	}
	return vectorToContainerValues(vector), nil
}

// dashboardQueryTimeout bounds dashboard-side reads of recording rules.
const dashboardQueryTimeout = 10 * time.Second

// QueryInstant runs a single instant query and returns the scalar/first-vector
// value. Returns 0 with no error if the query produces no samples.
func (c *Client) QueryInstant(ctx context.Context, expr string) (float64, error) {
	v, err := c.execInstant(ctx, expr, time.Now(), dashboardQueryTimeout)
	if err != nil {
		return 0, wrapQueryErr("instant query", expr, err)
	}
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
func (c *Client) QueryRange(ctx context.Context, expr string, r TimeRange, step string) ([]TimeValue, error) {
	allowed, probe := c.breaker.allow()
	if !allowed {
		return nil, ErrCircuitOpen
	}
	stp, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parse step %q: %w", step, err)
	}
	pr := prometheusv1.Range{Start: r.Start, End: r.End, Step: time.Duration(stp)}
	v, err := c.runRange(ctx, expr, pr, dashboardQueryTimeout, probe)
	if err != nil {
		return nil, fmt.Errorf("range query %q: %w", expr, err)
	}
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

// QueryByLabel runs an instant query and returns a map of label value to sample value.
func (c *Client) QueryByLabel(ctx context.Context, expr, label string) (map[string]float64, error) {
	v, err := c.execInstant(ctx, expr, time.Now(), dashboardQueryTimeout)
	if err != nil {
		return nil, wrapQueryErr("by-label query", expr, err)
	}
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
// labels joined with '|'. Samples missing any requested label are skipped.
func (c *Client) QueryByLabels(ctx context.Context, query string, labels ...string) (map[string]float64, error) {
	v, err := c.execInstant(ctx, query, time.Now(), dashboardQueryTimeout)
	if err != nil {
		return nil, wrapQueryErr("by-labels query", query, err)
	}
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
