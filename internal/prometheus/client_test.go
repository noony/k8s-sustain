package prometheus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestQueryInstantReturnsScalarFromVector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		q := r.Form.Get("query")
		if !strings.Contains(q, "k8s_sustain:cluster_cpu_savings_cores") {
			t.Fatalf("unexpected query: %q (raw=%s)", q, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3.2"]}]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.QueryInstant(context.Background(), "k8s_sustain:cluster_cpu_savings_cores")
	if err != nil {
		t.Fatal(err)
	}
	if v != 3.2 {
		t.Fatalf("got %v want 3.2", v)
	}
}

func TestQueryInstantEmptyVectorReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	v, err := c.QueryInstant(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("expected 0, got %v", v)
	}
}

func TestQueryByLabelMapsLabelValueToSample(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"policy":"prod"},"value":[1700000000,"1.5"]},
			{"metric":{"policy":"dev"},"value":[1700000000,"0.25"]}
		]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	out, err := c.QueryByLabel(context.Background(), "anything", "policy")
	if err != nil {
		t.Fatal(err)
	}
	if got := out["prod"]; got != 1.5 {
		t.Fatalf("prod: got %v want 1.5", got)
	}
	if got := out["dev"]; got != 0.25 {
		t.Fatalf("dev: got %v want 0.25", got)
	}
}

func TestQueryRangeReturnsTimeValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[[1700000000,"1"],[1700000060,"2"]]}
		]}}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-5 * time.Minute), End: now}
	c, _ := New(server.URL)
	out, err := c.QueryRange(context.Background(), "anything", tr, "1m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d points, want 2", len(out))
	}
	if out[0].Value != 1 || out[1].Value != 2 {
		t.Fatalf("unexpected values: %+v", out)
	}
}

func TestQueryByLabels_JoinsMultipleLabelsWithPipe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
	}))
	defer server.Close()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"namespace":"ns1","owner_name":"a"},"value":[0,"1"]},
			{"metric":{"namespace":"ns2","owner_name":"b"},"value":[0,"2"]},
			{"metric":{"namespace":"ns3"},"value":[0,"3"]}
		]}}`))
	})

	c, _ := New(server.URL)
	out, err := c.QueryByLabels(context.Background(), "anything", "namespace", "owner_name")
	if err != nil {
		t.Fatalf("QueryByLabels: %v", err)
	}
	if got := out["ns1|a"]; got != 1 {
		t.Errorf("ns1|a = %v, want 1", got)
	}
	if got := out["ns2|b"]; got != 2 {
		t.Errorf("ns2|b = %v, want 2", got)
	}
	if _, ok := out["ns3|"]; ok {
		t.Errorf("incomplete label series should be dropped, got entry for %q", "ns3|")
	}
	if len(out) != 2 {
		t.Errorf("expected 2 entries, got %d (%v)", len(out), out)
	}
}

// A per-label range read keys each series by the label's value and skips a
// series without it.
func TestQueryRangeByLabel_KeysEachSeriesByTheLabel(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"container":"app"},"values":[[1700000000,"0.5"],[1700000060,"0.75"]]},
			{"metric":{"container":"sidecar"},"values":[[1700000000,"0.1"]]},
			{"metric":{},"values":[[1700000000,"9"]]}
		]}}`))
	}))
	defer server.Close()

	now := time.Now()
	c, _ := New(server.URL)
	got, err := c.QueryRangeByLabel(context.Background(), "max by (container) (x)", "container", TimeRange{Start: now.Add(-5 * time.Minute), End: now}, "1m")
	if err != nil {
		t.Fatalf("QueryRangeByLabel: %v", err)
	}

	if query != "max by (container) (x)" {
		t.Errorf("query = %q, want it as given", query)
	}
	want := map[string][]TimeValue{
		"app":     {{Timestamp: time.Unix(1700000000, 0).UTC(), Value: 0.5}, {Timestamp: time.Unix(1700000060, 0).UTC(), Value: 0.75}},
		"sidecar": {{Timestamp: time.Unix(1700000000, 0).UTC(), Value: 0.1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestQueryRangeByLabel_BadStep(t *testing.T) {
	c, _ := New("http://127.0.0.1:1")
	now := time.Now()
	if _, err := c.QueryRangeByLabel(context.Background(), "x", "container", TimeRange{Start: now.Add(-time.Minute), End: now}, "nope"); err == nil {
		t.Fatal("expected an error for an invalid step")
	}
}

// The markers read the kill rule, never kube-state-metrics, summed over a
// window of exactly one step so each kill lands in one point, per identity
// and container.
func TestOOMKillsPerStep_ReadsTheKillRuleOverOneStep(t *testing.T) {
	got := OOMKillsPerStep(WorkloadSelector("ns", "Deployment", "web"), 5*time.Minute)

	want := `sum by (namespace, owner_kind, owner_name, container) (sum_over_time(k8s_sustain:container_oom_kills{namespace="ns",owner_kind="Deployment",owner_name="web"}[5m]))`
	if got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

// Each kill counted in a step is one event at that step.
func TestOOMEventsOf_OneEventPerKill(t *testing.T) {
	at := func(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

	got := OOMEventsOf("app", []TimeValue{{Timestamp: at(1700000000), Value: 0}, {Timestamp: at(1700000300), Value: 2}, {Timestamp: at(1700000600), Value: 1}})

	want := []OOMKill{
		{Timestamp: at(1700000300), Container: "app"},
		{Timestamp: at(1700000300), Container: "app"},
		{Timestamp: at(1700000600), Container: "app"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
}

// webShardQuery is a one-workload shard query, the smallest one that runs
// through execInstant with the client's query timeout.
const webShardQuery = `k8s_sustain:workload_max_pod_cpu:cores{namespace="ns",owner_kind="Deployment",owner_name=~"web"}`

// breakerFailures reads the breaker's failure counter under its mutex.
func breakerFailures(c *Client) int {
	c.breaker.mu.Lock()
	defer c.breaker.mu.Unlock()
	return c.breaker.failures
}

// hangingOOMServer returns an httptest server whose handler blocks until its
// request context is cancelled or the returned release channel is closed. The
// caller MUST close release at test end so blocked handler goroutines unwind
// and goleak stays happy.
func hangingOOMServer(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	return server, release
}

// TestExecInstant_GenuineErrorCountsOne asserts that a genuine Prometheus
// error (server 500, per-call context still live) on the execInstant path
// records exactly one breaker failure — unchanged from the old behaviour.
func TestExecInstant_GenuineErrorCountsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryShard(context.Background(), webShardQuery, time.Now())
	if err == nil {
		t.Fatal("expected an error from the failing query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a genuine query error, got %d", got)
	}
}

// TestExecInstant_PerCallTimeoutCountsOne asserts that the per-call
// queryTimeout deadline firing (a genuinely stalled Prometheus) on the
// execInstant path still records one breaker failure — the cause is
// context.DeadlineExceeded, a real failure attributable to Prometheus.
func TestExecInstant_PerCallTimeoutCountsOne(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL, WithQueryTimeout(50*time.Millisecond))
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryShard(context.Background(), webShardQuery, time.Now())
	if err == nil {
		t.Fatal("expected an error from the per-call timeout")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a per-call timeout, got %d", got)
	}
}

// TestExecInstant_ParentCancelCountsOne asserts that cancelling the parent
// context (a plain context.WithCancel, cause context.Canceled) while a query
// is in flight on the execInstant path still records exactly one breaker
// failure. This is a distinct branch from TestExecInstant_PerCallTimeoutCountsOne
// (cause context.DeadlineExceeded) and from
// TestExecInstant_OuterSiblingCancelCountsZero (cause a non-context sibling
// error) — recordQueryFailure treats DeadlineExceeded and Canceled as the same
// real-failure case, but that is exactly the assumption this test pins down.
//
// The handler signals `received` once the request lands, and the cancelling
// goroutine waits on that channel instead of sleeping — the cancel is
// guaranteed to land only once the request is actually in flight, so the test
// is deterministic under -race -shuffle=on.
func TestExecInstant_ParentCancelCountsOne(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(received)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-received
		cancel()
	}()

	_, err := c.QueryShard(ctx, webShardQuery, time.Now())
	if err == nil {
		t.Fatal("expected an error from the parent-context cancellation")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a parent cancellation, got %d", got)
	}
}

// TestExecInstant_OuterSiblingCancelCountsZero asserts that when an errgroup
// sibling fails, the collateral cancellation — whose cause is a
// NON-context error propagated through the WithTimeout child — records ZERO
// breaker failures on the execInstant path. The outage belongs to the sibling,
// which already counted its own failure. Before the fix every error counted,
// so one Prometheus outage recorded 2-3 failures per reconcile.
func TestExecInstant_OuterSiblingCancelCountsZero(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	siblingErr := errors.New("errgroup sibling failed")
	ctx, cancelCause := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancelCause(siblingErr)
	}()
	defer cancelCause(nil)

	_, err := c.QueryShard(ctx, webShardQuery, time.Now())
	if err == nil {
		t.Fatal("expected an error from the sibling cancellation")
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("expected 0 breaker failures for sibling collateral cancellation, got %d", got)
	}
}

// TestRunRange_GenuineErrorCountsOne is the runRange-path twin of
// TestExecInstant_GenuineErrorCountsOne.
func TestRunRange_GenuineErrorCountsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
	}))
	defer server.Close()

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryShardRange(context.Background(), webShardQuery, tr, time.Minute)
	if err == nil {
		t.Fatal("expected an error from the failing range query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a genuine range-query error, got %d", got)
	}
}

// TestRunRange_PerCallTimeoutCountsOne is the runRange-path twin of
// TestExecInstant_PerCallTimeoutCountsOne.
func TestRunRange_PerCallTimeoutCountsOne(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	c, _ := New(server.URL, WithQueryTimeout(50*time.Millisecond))
	c.breaker = newBreaker(10, time.Minute)

	_, err := c.QueryShardRange(context.Background(), webShardQuery, tr, time.Minute)
	if err == nil {
		t.Fatal("expected an error from the per-call timeout")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a range-query timeout, got %d", got)
	}
}

// TestRunRange_OuterSiblingCancelCountsZero is the runRange-path twin of
// TestExecInstant_OuterSiblingCancelCountsZero.
func TestRunRange_OuterSiblingCancelCountsZero(t *testing.T) {
	server, release := hangingOOMServer(t)
	defer server.Close()
	defer close(release)

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	siblingErr := errors.New("errgroup sibling failed")
	ctx, cancelCause := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancelCause(siblingErr)
	}()
	defer cancelCause(nil)

	now := time.Now()
	tr := TimeRange{Start: now.Add(-time.Hour), End: now}
	_, err := c.QueryShardRange(ctx, webShardQuery, tr, time.Minute)
	if err == nil {
		t.Fatal("expected an error from the sibling cancellation")
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("expected 0 breaker failures for sibling collateral cancellation, got %d", got)
	}
}

// TestErrgroupSiblings_SingleOutageCountsOnce: two queries share an errgroup,
// one genuinely fails (500) and the sibling hangs until the group cancels it.
// The genuine failure counts one breaker failure; the cancelled sibling counts
// zero — before the fix the sibling's context.Canceled also counted, so one
// outage recorded a failure per in-flight query.
func TestErrgroupSiblings_SingleOutageCountsOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.Contains(r.Form.Get("query"), "workload_max_pod_cpu"):
			// The genuine, independent failure.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"boom"}`))
		default:
			// Sibling query: block until the errgroup cancels gctx.
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	c, _ := New(server.URL)
	c.breaker = newBreaker(10, time.Minute)

	g, gctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		_, err := c.QueryShard(gctx, webShardQuery, time.Now())
		return err
	})
	g.Go(func() error {
		_, err := c.QueryShard(gctx, `k8s_sustain:workload_max_pod_memory:bytes{namespace="ns"}`, time.Now())
		return err
	})
	if err := g.Wait(); err == nil {
		t.Fatal("expected an error from the failing cpu query")
	}
	if got := breakerFailures(c); got != 1 {
		t.Fatalf("expected exactly 1 breaker failure for a single outage across errgroup siblings, got %d", got)
	}
}

func TestPing_OK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestPing_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c, _ := New(server.URL)
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected error on 500")
	}
}

func TestQuantileOverTimeUsesRangeVectorNotSubquery(t *testing.T) {
	got := QuantileOverTime(0.95, "k8s_sustain:workload_max_pod_cpu:cores",
		`{namespace="prod",owner_kind="Deployment",owner_name="api"}`, "7d")

	want := `quantile_over_time(0.95, k8s_sustain:workload_max_pod_cpu:cores{namespace="prod",owner_kind="Deployment",owner_name="api"}[7d])`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if strings.Contains(got, ":1m]") {
		t.Fatalf("expression still contains a 1m subquery step: %s", got)
	}
}

// A query that never reached Prometheus because it was cancelled while queued
// behind the in-flight semaphore must be distinguishable from one that ran and
// failed — they call for opposite operator responses — and must NOT count as a
// breaker failure, since a self-imposed wait says nothing about Prometheus.
func TestAcquireAbortWrapsSentinelAndSkipsBreaker(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	// Single slot, occupied by a query parked in the handler.
	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	var g errgroup.Group
	g.Go(func() error {
		_, qErr := c.QueryInstant(context.Background(), "up")
		return qErr
	})
	<-arrived // the slot is now genuinely held

	// This caller can never get a slot; its context is already done, so it
	// aborts in acquire rather than reaching Prometheus.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.QueryInstant(ctx, "up")

	if !errors.Is(err, ErrInflightQueueAborted) {
		t.Fatalf("expected ErrInflightQueueAborted, got %v", err)
	}
	// The underlying context cause must survive the wrapping too.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped context.Canceled, got %v", err)
	}
	if got := breakerFailures(c); got != 0 {
		t.Fatalf("breaker recorded %d failures; a self-imposed queue abort must record none", got)
	}

	// Unpark the held query so the goroutine and server shut down cleanly
	// (goleak runs in this package).
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatalf("parked query: %v", err)
	}
}

// TestWithMaxInflightBoundsConcurrentQueries confirms WithMaxInflight caps the
// number of requests the client has open against Prometheus at once, even
// when far more callers are trying to query concurrently.
//
// Deterministic by construction rather than by sleep: the handler reports its
// arrival on a buffered "arrived" channel before blocking on "release". The
// test drains exactly `limit` arrivals — proving `limit` requests are
// in-flight simultaneously — before closing "release" to let them all
// complete. Any (limit+1)-th request that reached the handler before the
// first `limit` finished would show up as an extra send on "arrived", which
// the fixed-size receive loop below would not drain, deadlocking the test
// instead of silently passing. A sleep-based version of this test would be a
// flake risk under -race/-shuffle and CI load; this version cannot flake on
// timing because it never depends on how long anything takes, only on how
// many requests reach the handler before "release" closes.
func TestWithMaxInflightBoundsConcurrentQueries(t *testing.T) {
	const limit = 2
	const callers = 8

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(limit))
	if err != nil {
		t.Fatal(err)
	}

	var g errgroup.Group
	for i := 0; i < callers; i++ {
		g.Go(func() error {
			_, qErr := c.QueryInstant(context.Background(), "up")
			return qErr
		})
	}

	// Wait for exactly `limit` requests to reach the handler. Since the
	// semaphore admits at most `limit` at a time and none can finish until
	// "release" closes, this proves peak in-flight has reached `limit`
	// without racing on inFlight/peak directly.
	for i := 0; i < limit; i++ {
		<-arrived
	}
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Fatalf("peak concurrent requests %d exceeded limit %d", peak, limit)
	}
	if peak < limit {
		t.Fatalf("peak concurrent requests %d never reached limit %d — semaphore may be over-restrictive", peak, limit)
	}
}

// The queue wait must be bounded by the CLIENT, not only by the caller's
// context. The controller's reconcile context carries no deadline, so before
// this bound existed a saturated semaphore parked the goroutine forever and
// ErrInflightQueueAborted was unreachable for exactly the caller that needed it
// most. The pre-existing abort test passes a context that is already cancelled,
// so it never exercised this case.
func TestAcquire_BoundsWaitWhenCallerHasNoDeadline(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()
	defer close(release)

	c, err := New(server.URL, WithMaxInflight(1), WithQueueTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the only slot with a query parked in the handler.
	go func() { _, _ = c.QueryInstant(context.Background(), "up") }()
	<-arrived

	// A caller with NO deadline of its own — the controller's shape.
	done := make(chan error, 1)
	go func() {
		_, qErr := c.QueryInstant(context.Background(), "up")
		done <- qErr
	}()

	select {
	case qErr := <-done:
		if !errors.Is(qErr, ErrInflightQueueAborted) {
			t.Fatalf("expected ErrInflightQueueAborted, got %v", qErr)
		}
		if !errors.Is(qErr, context.DeadlineExceeded) {
			t.Errorf("expected the wrapped cause to be DeadlineExceeded, got %v", qErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a deadline-less caller queued behind a saturated semaphore never returned: " +
			"the queue wait must be bounded by the client, not only by the caller's context")
	}

	if got := breakerFailures(c); got != 0 {
		t.Errorf("breaker recorded %d failures; a self-imposed queue abort must record none", got)
	}
}

// The breaker is checked before queuing, and the queue can be long. A query
// that waited while Prometheus went down should not spend a slot on a call
// already known to fail. The re-check must also not consume the half-open
// probe, and must not leak the slot it just took.
func TestAcquire_RejectsWhenBreakerIsOpenAfterQueuing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	for range defaultBreakerMaxFailures {
		c.breaker.failure()
	}

	// holdsProbe=false: a caller that passed allow() while the circuit was
	// closed and then queued while it tripped. This is the caller the re-check
	// was written for, and it must still bail.
	if _, err := c.acquire(context.Background(), false); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("acquire with an open breaker: got %v, want ErrCircuitOpen", err)
	}
	if len(c.sem) != 0 {
		t.Error("acquire leaked its in-flight slot when rejecting on an open breaker")
	}
	// The rejection must be an observation, not a claim: the probe a cooldown
	// expiry grants has to still be available to a real caller.
	if !c.breaker.isOpen() {
		t.Error("the re-check consumed the half-open probe; it must observe, not claim")
	}
}

// The counterpart to the test above, and the half that keeps the circuit able
// to close: the caller holding the half-open probe must NOT be rejected by the
// deadline its own allow() just advanced. Rejecting it means the probe never
// reaches Prometheus, success() is never called, and the circuit is open
// forever.
func TestAcquire_AdmitsTheHalfOpenProbeHolder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	c, err := New(server.URL, WithMaxInflight(1))
	if err != nil {
		t.Fatal(err)
	}
	for range defaultBreakerMaxFailures {
		c.breaker.failure()
	}
	// The circuit is open and, after allow() hands out the probe, openUntil is
	// advanced — so isOpen() is true for the probe holder by construction.
	if !c.breaker.isOpen() {
		t.Fatal("precondition: the circuit must be open")
	}

	release, err := c.acquire(context.Background(), true)
	if err != nil {
		t.Fatalf("acquire must admit the half-open probe holder, got %v", err)
	}
	release()
	if len(c.sem) != 0 {
		t.Error("release did not return the in-flight slot")
	}
}
