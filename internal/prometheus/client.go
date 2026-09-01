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

// quantileOverTimeExpr builds `quantile_over_time(<q>, <rule>{selector}[<window>])`,
// the percentile used by the recommendation queries.
//
// This reads the rule as a plain RANGE VECTOR, not as a `[window:1m]` subquery.
// The rules it targets (k8s_sustain:workload_max_pod_cpu:cores and friends) are
// themselves materialised at `interval: 1m`, so a 1m-step subquery would
// re-evaluate, point by point, a series that already has exactly that
// resolution — paying window/1m nested evaluations for no additional fidelity.
//
// The two are equivalent but not bit-identical: a subquery carries the last
// sample forward across gaps up to the 5m staleness lookback, whereas a range
// vector reports the gap. Over a multi-thousand-sample window the effect on a
// percentile is noise, and this is validated against real data in the kind
// scenario harness before release.
func quantileOverTimeExpr(quantile float64, rule, selector, window string) string {
	return fmt.Sprintf("quantile_over_time(%.2f, %s%s[%s])", quantile, rule, selector, window)
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
//
// The in-flight semaphore is acquired BEFORE the per-call context.WithTimeout
// is created, not after. If it were acquired after, time spent queueing for a
// semaphore slot would be charged against the query's own timeout budget —
// under load that turns the throttle into a failure amplifier, timing out
// queries that never actually got a chance to run against Prometheus. The
// queue wait is bounded independently inside acquire, so keeping it out of this
// budget does not make it unbounded.
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

// runRange runs a range query through the circuit breaker without the
// breaker.allow() gate, for callers that must parse durations or share a single
// allow() gate before querying. It applies the per-call timeout, records
// failure/success (failures attributed via recordQueryFailure), and logs
// warnings — the same preamble execInstant inlines.
//
// holdsProbe is the second return value of the caller's own breaker.allow()
// call: it says whether this query IS the half-open probe. It has to be
// threaded down rather than re-derived, because acquire's re-check would
// otherwise reject the probe holder using the deadline allow() set for it —
// see breaker.allow.
//
// As in execInstant, the in-flight semaphore is acquired BEFORE
// context.WithTimeout so time spent queueing for a slot is never charged
// against the query's own timeout budget — see execInstant's comment for why
// getting this ordering backwards turns the throttle into a failure amplifier.
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
	queueTimeout time.Duration
	// sem bounds concurrent in-flight queries. nil disables the bound.
	sem chan struct{}
	// transport holds the auth/TLS settings from WithTransportConfig, and
	// transportSet records that the option was used at all — an explicitly
	// passed zero TransportConfig must still conflict with WithRoundTripper.
	transport    TransportConfig
	transportSet bool
	// roundTripper is the explicit transport from WithRoundTripper, if any.
	roundTripper http.RoundTripper
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

// WithMaxInflight bounds the number of queries this client will have in flight
// at once. Prometheus's own --query.max-concurrency defaults to 20 and is
// shared with every other consumer of that server (dashboards, alerting), so
// k8s-sustain deliberately stays well under it rather than saturating the gate.
// n <= 0 leaves the client unbounded.
//
// The default (8, see --prometheus-max-inflight) is sized for the current
// one-query-per-workload pattern, where a reconcile burst can put hundreds of
// small queries in flight. Batched shard queries change that profile — far
// fewer queries, each individually heavier and slower — so this value is worth
// re-deriving rather than assuming once batching ships.
func WithMaxInflight(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.sem = make(chan struct{}, n)
		}
	}
}

// WithQueueTimeout overrides how long a query may wait for an in-flight slot
// before being abandoned. See defaultQueueTimeout for why this is bounded
// separately from the query timeout.
func WithQueueTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.queueTimeout = d
		}
	}
}

// ErrInflightQueueAborted reports that a query never reached Prometheus because
// its context ended while it was queued behind the in-flight semaphore.
//
// This is deliberately distinguishable from a context error raised by a query
// that genuinely ran: the two are operationally opposite. The first means
// k8s-sustain throttled itself and Prometheus may be perfectly healthy; the
// second is evidence about Prometheus. Without a sentinel an operator reading
// logs cannot tell "we are backpressured by our own cap" from "Prometheus is
// slow", and would reach for the wrong remedy. Note that this path also
// bypasses breaker accounting by construction — a self-imposed wait must never
// trip the circuit breaker.
var ErrInflightQueueAborted = errors.New("prometheus: aborted while queued for an in-flight slot")

// acquire takes a slot from the in-flight semaphore, returning the release
// func. Honours ctx cancellation so a caller that has already blown its
// deadline does not queue behind a saturated Prometheus.
//
// holdsProbe says the caller won the breaker's half-open probe (the second
// return value of breaker.allow). Such a caller is exempt from the post-queue
// re-check below — see the comment there.
func (c *Client) acquire(ctx context.Context, holdsProbe bool) (func(), error) {
	if c.sem == nil {
		return func() {}, nil
	}
	// The wait gets its OWN deadline rather than inheriting only the caller's.
	// The controller's reconcile context carries no deadline at all, so without
	// this the queue wait is unbounded: a saturated semaphore parks the
	// goroutine indefinitely and ErrInflightQueueAborted is unreachable unless
	// the caller happened to bring a deadline of its own.
	//
	// This deadline is deliberately NOT the query timeout, and the two must not
	// be merged: queue time is not query time, and charging it against the
	// query's budget turns the throttle into a failure amplifier, timing out
	// queries that never got a chance to run (see execInstant's doc comment).
	// It only has to guarantee termination, so it is generous — a query still
	// queued this long has already missed the reconcile it belonged to, and the
	// next cycle will reissue it.
	ctx, cancel := context.WithTimeout(ctx, c.queueTimeout)
	defer cancel()

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrInflightQueueAborted, ctx.Err())
	}

	// The breaker was checked before queuing, and the queue can be long. If
	// Prometheus went down while this query waited, sending it anyway spends a
	// slot on a call that is already known to fail. isOpen re-checks WITHOUT
	// consuming the half-open probe: this is a "should I still bother" test,
	// not a claim on the one probe a cooldown expiry grants.
	//
	// The probe holder is exempt, and that exemption is what keeps the circuit
	// able to close at all. allow() hands out the probe by ADVANCING openUntil
	// (so an abandoned probe self-heals), so isOpen necessarily returns true for
	// the very caller the cooldown just admitted. Without this guard that caller
	// returns ErrCircuitOpen without ever reaching Prometheus, success() is
	// never called, and the breaker stays open forever — permanently, across
	// every later cooldown, even after Prometheus recovers.
	//
	// The check still does its job for the caller it was written for: one that
	// passed allow() while the circuit was CLOSED and then queued while it
	// tripped. That caller holds no probe, so it still bails here.
	if !holdsProbe && c.breaker.isOpen() {
		<-c.sem
		return nil, ErrCircuitOpen
	}
	return func() { <-c.sem }, nil
}

// New creates a Prometheus client targeting addr (e.g. "http://prometheus:9090").
//
// Options are applied BEFORE the underlying api.Client is built, because
// WithTransportConfig / WithRoundTripper decide the RoundTripper that
// api.Config is constructed with — it cannot be swapped in afterwards.
// New(addr) with no options passes no RoundTripper at all, so it is identical
// to the pre-authentication behaviour: api.DefaultRoundTripper, no credentials.
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

// oomMetricNames are the three recording rules that jointly make up the OOM
// signal. They share the {namespace,owner_kind,owner_name,container} label
// set, so one __name__ regex selector fetches all of them in a single query.
var oomMetricNames = []string{
	MetricWorkloadOOM24h,
	MetricContainerPeakMemory24hBytes,
	MetricContainerOOMLimit24hBytes,
}

// oomSignalSelector builds the combined selector
// `{__name__=~"a|b|c",namespace=…,owner_kind=…,owner_name=…}`.
//
// The names are interpolated into an RE2 alternation unescaped. That is safe
// only while every name in oomMetricNames consists of characters RE2 treats as
// literals — today they are `[a-z_]` plus ':', which is not a metacharacter.
// Renaming a recording rule to include '.', '(', '+' or similar would silently
// widen what this selector matches, with no compile or test failure to catch
// it. TestOOMSignalSelectorUsesLiteralAlternation pins that invariant.
func oomSignalSelector(namespace, ownerKind, ownerName string) string {
	return fmt.Sprintf("{__name__=~%q,namespace=%q,owner_kind=%q,owner_name=%q}",
		strings.Join(oomMetricNames, "|"), namespace, ownerKind, ownerName)
}

// foldOOMVector reproduces, in Go, the aggregation the three separate probes
// used to ask Prometheus for: `sum by (container)` over the OOM counts and
// `max by (container)` over the peak and OOM-time limit. The aggregation exists
// to collapse duplicate series emitted by multiple kube-state-metrics replicas.
//
// Presence is tracked explicitly rather than inferred from a non-zero value:
// ComputeContainerRec gates memory emission on whether the container has a key
// in PeakMemoryBytes, so a legitimately-zero sample must still create the key.
func foldOOMVector(vec model.Vector) OOMSignal {
	sig := OOMSignal{
		OOMCounts:       ContainerValues{},
		PeakMemoryBytes: ContainerValues{},
		OOMLimitBytes:   ContainerValues{},
	}
	// The `!ok` is load-bearing: do NOT simplify this to `if v > m[key]`.
	// That form never stores a first sample whose value is 0 (because 0 > 0 is
	// false), so the key is never created — and ComputeContainerRec gates
	// memory emission on key PRESENCE, not value. A container reporting a
	// legitimate zero peak would silently stop receiving recommendations.
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

// QueryWorkloadOOMSignal returns the recent per-container OOM counts (24h) and
// the peak per-container memory working-set bytes observed alongside them, plus
// the cgroup limit in force at OOM time. Used as a floor signal: if a container
// OOM'd, never recommend its memory below max(peak, current).
//
// All three metric families are fetched in ONE query via a __name__ regex
// selector and folded client-side (see foldOOMVector). This replaces three
// concurrent probes, and with them the per-call atomic that existed solely to
// guarantee at-most-one breaker failure across those probes — a single query
// through execInstant records at most one failure by construction.
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

// defaultQueryTimeout is the default per-query timeout used when WithQueryTimeout
// is not passed to New. Sized for background controller reconciles; the webhook
// overrides this with a tighter value.
const defaultQueryTimeout = 30 * time.Second

// defaultQueueTimeout bounds how long a query waits for an in-flight slot.
//
// It exists to guarantee termination, not to be aggressive. The controller's
// reconcile context has no deadline, so without a bound here a saturated
// semaphore parks a goroutine forever. Sized well above any legitimate wait —
// with the default 8 slots and a 30s query timeout, even a fully backed-up
// client drains a queue of tens of queries inside this window — so it converts
// "hangs indefinitely" into "fails visibly" without rejecting queries that
// would have run.
//
// Separate from defaultQueryTimeout on purpose: queue time is not query time.
// Sharing one budget would make a busy client time out queries that never
// reached Prometheus at all.
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
