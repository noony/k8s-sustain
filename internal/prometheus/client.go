package prometheus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// logWarnings emits Prometheus query Warnings at log level 0 so callers can
// notice partial results, hit cardinality limits, or remote-read failures
// instead of silently computing recommendations from incomplete data.
func logWarnings(ctx context.Context, expr string, warnings prometheusv1.Warnings) {
	if len(warnings) == 0 {
		return
	}
	log.FromContext(ctx).Info("prometheus query returned warnings", "expr", expr, "warnings", warnings)
}

// ContainerValues maps container name → metric value (cores for CPU, bytes for memory).
type ContainerValues map[string]float64

// workloadSelector builds the shared `{namespace=...,owner_kind=...,owner_name=...}`
// PromQL label-selector fragment used to scope queries to a single workload.
// The output is byte-identical to the inline `%q`-quoted selectors it replaces.
func workloadSelector(namespace, ownerKind, ownerName string) string {
	return fmt.Sprintf("{namespace=%q,owner_kind=%q,owner_name=%q}", namespace, ownerKind, ownerName)
}

// quantileOverTimeExpr builds `quantile_over_time(<q>, <rule>{selector}[<window>:1m])`,
// the per-instant sub-query percentile used by the recommendation queries.
func quantileOverTimeExpr(quantile float64, rule, selector, window string) string {
	return fmt.Sprintf("quantile_over_time(%.2f, %s%s[%s:1m])", quantile, rule, selector, window)
}

// avgByContainer wraps an inner expression in `avg by (container) (...)`.
func avgByContainer(inner string) string {
	return fmt.Sprintf("avg by (container) (%s)", inner)
}

// maxByContainer wraps an inner expression in `max by (container) (...)`.
func maxByContainer(inner string) string {
	return fmt.Sprintf("max by (container) (%s)", inner)
}

// recordQueryFailure attributes a single failed query to the circuit breaker
// using the same rules as QueryWorkloadOOMSignal. ctx must be the per-call
// WithTimeout child the query ran under:
//   - Genuine query error (ctx still live): a real Prometheus failure, counted.
//   - Per-call timeout or parent cancellation (context.Cause is a context
//     sentinel): counted, parity with QueryWorkloadOOMSignal's documented rules.
//   - Collateral abort from an errgroup sibling failing (these queries run
//     inside errgroups in recommender/build.go): counted ZERO times. The
//     sibling's error propagates through the WithTimeout child as a NON-context
//     cause, so the outage is attributed to the sibling — which already counted
//     its own failure — not to Prometheus.
func (c *Client) recordQueryFailure(ctx context.Context) {
	if ctx.Err() == nil {
		c.breaker.failure()
		return
	}
	if cause := context.Cause(ctx); errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		c.breaker.failure()
	}
}

// execInstant runs an instant query through the circuit breaker: it gates on
// breaker.allow(), applies the given per-call timeout, records failure/success
// (failures attributed via recordQueryFailure), and logs warnings — exactly the
// preamble the query methods used to inline. On an open breaker it returns
// ErrCircuitOpen with a nil value; on a query error it returns the raw API
// error (callers wrap it with their own prefix).
func (c *Client) execInstant(ctx context.Context, expr string, ts time.Time, timeout time.Duration) (model.Value, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
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

// runRange runs a range query through the circuit breaker without the
// breaker.allow() gate, for callers that must parse durations or share a single
// allow() gate before querying. It applies the per-call timeout, records
// failure/success (failures attributed via recordQueryFailure), and logs
// warnings — the same preamble execInstant inlines.
func (c *Client) runRange(ctx context.Context, expr string, r prometheusv1.Range, timeout time.Duration) (model.Value, error) {
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

// vectorToContainerValues unpacks an instant-query vector into per-container
// values, dropping samples whose `container` label is empty.
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
}

// Default circuit-breaker tuning: trip after 5 consecutive failures,
// stay open for 30 seconds. Sized so a sustained Prometheus outage opens the
// breaker within a couple of reconcile passes without over-counting a single
// outage: every query path attributes at most one breaker failure per genuine
// error or per-call timeout, and collateral errgroup-sibling cancellations
// count zero (see recordQueryFailure and QueryWorkloadOOMSignal), so a few
// stuck reconciles (each ≈ one queryTimeout long) reach the threshold.
const (
	defaultBreakerMaxFailures = 5
	defaultBreakerCooldown    = 30 * time.Second
)

// Option configures a Client at construction time.
type Option func(*Client)

// WithQueryTimeout overrides the default per-query timeout. Callers with a
// tight upstream budget (e.g. the admission webhook, which must respond within
// the apiserver's webhook timeout) should set this to a value that fits within
// that budget; the controller uses the default which is sized for background
// reconciles.
func WithQueryTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.queryTimeout = d
		}
	}
}

// New creates a Prometheus client targeting addr (e.g. "http://prometheus:9090").
func New(addr string, opts ...Option) (*Client, error) {
	c, err := api.NewClient(api.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	cli := &Client{
		api:          prometheusv1.NewAPI(c),
		breaker:      newBreaker(defaultBreakerMaxFailures, defaultBreakerCooldown),
		queryTimeout: defaultQueryTimeout,
	}
	for _, opt := range opts {
		opt(cli)
	}
	return cli, nil
}

// QueryCPUByContainer returns per-container CPU usage (cores) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_cpu_usage_by_workload:rate1m recording rule.
func (c *Client) QueryCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerCPUUsageByWorkloadRate1m, workloadSelector(namespace, ownerKind, ownerName), window))
	return c.queryByContainer(ctx, expr)
}

// QueryMemoryByContainer returns per-container memory working set (bytes) at the given quantile,
// averaged across pods of the workload, over the specified window.
// Relies on the k8s_sustain:container_memory_by_workload:bytes recording rule.
func (c *Client) QueryMemoryByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerMemoryByWorkloadBytes, workloadSelector(namespace, ownerKind, ownerName), window))
	return c.queryByContainer(ctx, expr)
}

// QueryWorkloadCPUByContainer returns the per-pod CPU recommendation basis (cores)
// per container: the given-quantile over the window of the busiest replica's CPU
// rate at each instant. Reads the k8s_sustain:workload_max_pod_cpu:cores recording
// rule, which collapses per-pod usage to the hottest live pod, so the result is a
// genuine percentile that covers the busiest replica — no replica division needed.
func (c *Client) QueryWorkloadCPUByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, window string) (ContainerValues, error) {
	expr := quantileOverTimeExpr(quantile, MetricWorkloadMaxPodCPUCores, workloadSelector(namespace, ownerKind, ownerName), window)
	return c.queryByContainer(ctx, expr)
}

// QueryWorkloadMemoryByContainer returns the per-pod memory recommendation basis
// (bytes) per container: the given-quantile over the window of the busiest
// replica's memory working set at each instant. Reads the
// k8s_sustain:workload_max_pod_memory:bytes recording rule. Same per-pod-percentile
// semantics as QueryWorkloadCPUByContainer.
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

// ContainerTimeSeries maps container name → time-series data points.
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

// QueryCPULimitRangeByContainer returns per-container CPU limit time-series (cores).
// Reads the per-pod cgroup limit (which the webhook updates on pod creation),
// not the workload spec — the dashboard needs the value pods are actually
// running with.
func (c *Client) QueryCPULimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerCPULimitsByWorkloadCores, namespace, ownerKind, ownerName, r, step)
}

// QueryMemoryLimitRangeByContainer returns per-container memory limit time-series (bytes).
func (c *Client) QueryMemoryLimitRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	return c.queryMaxByContainerForWorkload(ctx, MetricContainerMemoryLimitsByWorkloadBytes, namespace, ownerKind, ownerName, r, step)
}

// queryMaxByContainerForWorkload runs `max by (container) (<rule>{workload labels})`
// against a recording rule and returns the per-container time-series.
func (c *Client) queryMaxByContainerForWorkload(ctx context.Context, ruleName, namespace, ownerKind, ownerName string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := maxByContainer(ruleName + workloadSelector(namespace, ownerKind, ownerName))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryCPURecommendationRangeByContainer returns per-container sliding-window CPU recommendation
// time-series (cores) — at each step, the quantile is computed over the trailing recWindow.
func (c *Client) QueryCPURecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerCPUUsageByWorkloadRate1m, workloadSelector(namespace, ownerKind, ownerName), recWindow))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// QueryMemoryRecommendationRangeByContainer returns per-container sliding-window memory recommendation
// time-series (bytes) — at each step, the quantile is computed over the trailing recWindow.
func (c *Client) QueryMemoryRecommendationRangeByContainer(ctx context.Context, namespace, ownerKind, ownerName string, quantile float64, recWindow string, r TimeRange, step string) (ContainerTimeSeries, error) {
	expr := avgByContainer(quantileOverTimeExpr(quantile, MetricContainerMemoryByWorkloadBytes, workloadSelector(namespace, ownerKind, ownerName), recWindow))
	return c.queryRangeByContainer(ctx, expr, r, step)
}

// OOMSignal carries the OOM context for a single workload over the past 24h.
type OOMSignal struct {
	// OOMCounts is the per-container OOM event count over the past 24h.
	// Per-container so the recommender only floors the memory of containers
	// that actually OOMed — an innocent sidecar in the same pod keeps its
	// pure percentile recommendation.
	OOMCounts       ContainerValues
	PeakMemoryBytes ContainerValues
	// OOMLimitBytes is the cgroup memory limit observed at the moment a
	// recent OOM event fired, per container. Used by the recommender as a
	// bump anchor when peak working-set is unreliable (cgroup v2,
	// sub-scrape spikes can hide the real high-water mark).
	OOMLimitBytes ContainerValues
}

// TotalOOMs sums the per-container OOM counts into the workload-level count.
// Used by consumers that need workload-level recency (e.g. the young-workload
// age-gate bypass — a workload that OOMed anywhere is not too young to have data).
func (s OOMSignal) TotalOOMs() float64 {
	var total float64
	for _, v := range s.OOMCounts {
		total += v
	}
	return total
}

// recordOnce records at most one breaker failure across all probes of a single
// QueryWorkloadOOMSignal call. The shared counted flag is flipped with a
// compare-and-swap so that whichever caller (an in-goroutine genuine error or
// the post-Wait deadline/cancel inspection) gets there first counts, and every
// later caller is a no-op. This is what makes the at-most-one-per-call contract
// hold in every interleaving.
func (c *Client) recordOnce(counted *atomic.Bool) {
	if counted.CompareAndSwap(false, true) {
		c.breaker.failure()
	}
}

// QueryWorkloadOOMSignal returns the recent per-container OOM counts (24h) and
// the peak per-container memory working-set bytes observed alongside them. Used
// as a floor signal: if a container OOM'd, never recommend its memory below
// max(peak, current).
//
// The three probes run concurrently under a shared per-call timeout, so several
// can error at once. To keep the circuit breaker honest this method records AT
// MOST ONE breaker failure per call, in every interleaving:
//   - Genuine independent probe error (group context not yet cancelled):
//     counted once, in the goroutine that observed it.
//   - Shared queryTimeout deadline: counted once (a real Prometheus failure).
//   - Parent context cancellation (context.Canceled cause): counted once, for
//     parity with the old sequential code where the first failing query counted.
//   - Collateral abort from an OUTER errgroup sibling failing (this method runs
//     inside errgroups in recommender/build.go and dashboard/recommendations.go):
//     counted ZERO times. The outer cancellation cause propagates through the
//     WithTimeout child as a NON-context error, so it is attributed to the outer
//     sibling, not to Prometheus.
//
// The single count is enforced by a per-call atomic flag shared by the probe
// goroutines and the post-Wait inspection (see recordOnce). Each goroutine only
// counts a genuine independent error — one observed while the group context is
// still live (gctx.Err() == nil). Every cancellation-driven abort (shared
// deadline, parent cancel, outer-sibling collateral) is left to the post-Wait
// inspection of context.Cause on the WithTimeout child, which counts only the
// context sentinels and ignores the non-context outer-sibling cause.
func (c *Client) QueryWorkloadOOMSignal(ctx context.Context, namespace, ownerKind, ownerName string) (OOMSignal, error) {
	if !c.breaker.allow() {
		return OOMSignal{}, ErrCircuitOpen
	}
	ctx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()
	// Flipped once when any failure is attributed to this call; shared by the
	// probe goroutines and the post-Wait inspection below.
	var counted atomic.Bool

	// Single evaluation timestamp so all three probes share a consistent
	// snapshot of the Prometheus TSDB.
	now := time.Now()

	selector := workloadSelector(namespace, ownerKind, ownerName)
	oomExpr := fmt.Sprintf("sum by (container) (%s%s)", MetricWorkloadOOM24h, selector)
	// Use the dedicated peak rule (kernel high-water + OOM-scoped limit fallback).
	// Working-set sampled at scrape interval misses sub-second spikes that
	// trigger the kill — `container_memory_max_usage_bytes` (cgroup v1) and
	// `container_memory_peak_working_set_bytes` (cgroup v2) survive across scrape gaps.
	peakExpr := maxByContainer(MetricContainerPeakMemory24hBytes + selector)
	// OOM-time memory limit. Independent rule: failures here are non-fatal in the
	// sense that bumping just won't fire, but the existing peak floor still works.
	limitExpr := maxByContainer(MetricContainerOOMLimit24hBytes + selector)

	// The three probes are independent and share the single breaker.allow() gate
	// above plus the shared timeout context, so run them concurrently. Each
	// goroutine writes only its own result variable; results are combined after
	// Wait(). Each query keeps the same per-query breaker.success()/failure()
	// and warning-logging semantics as the original sequential code.
	var (
		oomCounts = ContainerValues{}
		peaks     = ContainerValues{}
		limits    = ContainerValues{}
	)
	g, gctx := errgroup.WithContext(ctx)
	// recordProbeError counts a genuine independent probe error exactly once.
	// A failure observed while the group context is still live (gctx.Err() ==
	// nil) is a real, independent Prometheus error and is counted here. Any
	// cancellation-driven abort (shared deadline, parent cancel, or an
	// outer-sibling collateral cancel) leaves gctx non-nil and is deferred to
	// the post-Wait inspection, which knows how to distinguish them.
	recordProbeError := func() {
		if gctx.Err() == nil {
			c.recordOnce(&counted)
		}
	}
	// queryProbe builds a goroutine that runs one probe query, attributes its
	// error exactly once, logs warnings, and stores the parsed vector into
	// target. The three probes differ only by expression, label, and target.
	queryProbe := func(expr, label string, target *ContainerValues) func() error {
		return func() error {
			res, warnings, err := c.api.Query(gctx, expr, now)
			if err != nil {
				recordProbeError()
				return fmt.Errorf("prometheus %s probe %q: %w", label, expr, err)
			}
			c.breaker.success()
			logWarnings(gctx, expr, warnings)
			if vec, ok := res.(model.Vector); ok {
				*target = vectorToContainerValues(vec)
			}
			return nil
		}
	}
	g.Go(queryProbe(oomExpr, "oom", &oomCounts))
	g.Go(queryProbe(peakExpr, "peak", &peaks))
	g.Go(queryProbe(limitExpr, "oom-limit", &limits))

	if err := g.Wait(); err != nil {
		// A cancellation-driven abort (shared deadline, parent cancel, or an
		// outer-sibling collateral cancel) is not counted in-goroutine; settle
		// it here from the WithTimeout child's cause. A context sentinel
		// (DeadlineExceeded/Canceled) is a real failure attributable to this
		// call and counts once; a non-context cause is the outer sibling's
		// failure (propagated through this child) and counts zero. recordOnce is
		// CAS-guarded, so if a genuine probe error already counted, this is a
		// no-op.
		if cause := context.Cause(ctx); errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
			c.recordOnce(&counted)
		}
		return OOMSignal{}, err
	}
	return OOMSignal{OOMCounts: oomCounts, PeakMemoryBytes: peaks, OOMLimitBytes: limits}, nil
}

// OOMEvent represents a single OOM kill event for a container.
type OOMEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Container string    `json:"container"`
	Pod       string    `json:"pod"`
}

// QueryOOMKillEvents returns OOM kill events for a workload over the specified range.
// Uses kube_pod_container_status_restarts_total joined with
// kube_pod_container_status_last_terminated_reason{reason="OOMKilled"} to detect
// restart events caused by OOM kills.
func (c *Client) QueryOOMKillEvents(ctx context.Context, namespace, ownerKind, ownerName string, r TimeRange, step string) ([]OOMEvent, error) {
	stepDur, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parsing step %q: %w", step, err)
	}

	// `increase()` needs ≥2 samples inside its lookback range to return anything.
	// kube-state-metrics typically scrapes every 30–60s, so when the UI picks a
	// short step (e.g. step=1m for the 1h window), `increase(restarts[step])`
	// silently drops real OOM kills because only one scrape lands in the
	// lookback. Floor the lookback at 5m so the query is resilient to ordinary
	// scrape intervals regardless of the chart step.
	lookback := max(time.Duration(stepDur), 5*time.Minute)
	lookbackExpr := model.Duration(lookback).String()

	// Per-event detection: increase(restarts[lookback]) gated on
	// last_terminated_reason==OOMKilled. Each restart bumps the value
	// (0→1, 1→2, …); the value-bump dedup below turns each bump into one
	// event. A max_over_time fallback on the reason would keep the signal
	// positive for the entire CrashLoopBackOff tail and collapse subsequent
	// OOMs into one continuous run — fine for the workload_oom_24h boolean,
	// not for per-event markers.
	//
	// Both sides are aggregated with `max by (namespace, pod, container)` to
	// drop kube-state-metrics scrape-target labels (instance, service, …).
	// Without this, a KSM rollout inside the chart window produces overlapping
	// series for the same pod, and the `group_left()` join fails with
	// "many-to-many matching not allowed", aborting the whole query — which
	// the dashboard then swallows as an empty OOM list.
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

	if !c.breaker.allow() {
		// Non-fatal: skip OOM lookup while breaker is open.
		return nil, nil
	}

	result, err := c.runRange(ctx, expr, prometheusv1.Range{
		Start: r.Start,
		End:   r.End,
		Step:  time.Duration(stepDur),
	}, c.queryTimeout)
	if err != nil {
		// Non-fatal: OOM data may not be available (missing kube-state-metrics etc.),
		// so the dashboard still renders. runRange already recorded the breaker
		// failure; swallowing the error here is the documented contract.
		return nil, nil //nolint:nilerr // intentional non-fatal swallow, see comment above
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, nil
	}

	// Value-bump dedup per (pod, container). `increase()` smears each
	// restart across the lookback (flat 1, 1, 1), but a SECOND OOM inside
	// the lookback bumps the value up (1 → 2). Emit on each upward step;
	// skip flat smears and the downward tail as the counter ages out. The
	// 0.5 threshold absorbs extrapolation noise without missing the next
	// integer bump (~1.0).
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
	if !c.breaker.allow() {
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
	}, c.queryTimeout)
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

// defaultQueryTimeout is the default per-query timeout used when WithQueryTimeout
// is not passed to New. Sized for background controller reconciles; the webhook
// overrides this with a tighter value.
const defaultQueryTimeout = 30 * time.Second

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
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	stp, err := model.ParseDuration(step)
	if err != nil {
		return nil, fmt.Errorf("parse step %q: %w", step, err)
	}
	pr := prometheusv1.Range{Start: r.Start, End: r.End, Step: time.Duration(stp)}
	v, err := c.runRange(ctx, expr, pr, dashboardQueryTimeout)
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

// QueryByLabel runs an instant query and returns a map of label-value -> sample value.
// Used for per-policy and per-workload aggregates.
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
// labels joined with '|'. Samples missing any of the requested labels are
// skipped. Useful when several labels jointly identify a series.
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
