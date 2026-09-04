package recommender

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// batchTestRsCfg is the shared fixture for FetchWorkloadInputsBatch tests: a
// short, valid window on both resources so BuildShards' windowMinutes stays
// small and every candidate fits comfortably inside one shard at the generous
// maxSamples budget the tests pass.
func batchTestRsCfg() sustainv1alpha1.ResourcesConfigs {
	p95 := int32(95)
	return sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{Window: "1h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
		Memory: sustainv1alpha1.ResourceConfig{Window: "1h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
	}
}

// queryCounter is a concurrency-safe counter of Prometheus query attempts,
// bucketed by a caller-supplied category string. FetchWorkloadInputsBatch
// runs the shards within each of its CPU/memory/OOM passes concurrently
// (bounded by shardFetchConcurrency), and a shard's fallback path in turn
// calls FetchWorkloadInputs, which fans its own CPU/mem/OOM queries out via
// errgroup -- so the test server's handler must be safe for concurrent use
// regardless of which of those layers is issuing the request.
type queryCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newQueryCounter() *queryCounter { return &queryCounter{counts: map[string]int{}} }

func (q *queryCounter) inc(category string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.counts[category]++
}

func (q *queryCounter) get(category string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counts[category]
}

// classifyQuery buckets a raw PromQL query string into "shard-cpu",
// "shard-mem", "shard-oom", "workload-cpu", "workload-mem", or
// "workload-oom", mirroring the metric names and selector shapes used by
// build.go/batch.go/shard.go.
func classifyQuery(q string) string {
	isShard := strings.Contains(q, `owner_name=~`)
	switch {
	case strings.Contains(q, "workload_max_pod_cpu"):
		if isShard {
			return "shard-cpu"
		}
		return "workload-cpu"
	case strings.Contains(q, "workload_max_pod_memory"):
		if isShard {
			return "shard-mem"
		}
		return "workload-mem"
	case strings.Contains(q, "__name__"):
		if isShard {
			return "shard-oom"
		}
		return "workload-oom"
	default:
		return "unknown: " + q
	}
}

var ownerNameRE = regexp.MustCompile(`owner_name="([^"]+)"`)

// TestFetchWorkloadInputsBatch_BatchesIntoSingleQueries is the load-bearing
// assertion for this whole task: two workloads in the same (namespace,
// owner_kind) shard must cost exactly one CPU query, one memory query and one
// OOM query -- not two of each. Without this assertion the implementation
// could silently degrade to one query per workload (defeating the entire
// point of sharding) and every other test in this file would still pass.
func TestFetchWorkloadInputsBatch_BatchesIntoSingleQueries(t *testing.T) {
	qc := newQueryCounter()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		qc.inc(classifyQuery(r.Form.Get("query")))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	cands := []promclient.ShardCandidate{
		{Identity: promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}, Containers: 1},
		{Identity: promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}, Containers: 1},
	}

	out, stats := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	if len(out) != 2 {
		t.Fatalf("expected 2 entries in result, got %d: %+v", len(out), out)
	}
	if len(stats.Failures) != 0 {
		t.Errorf("expected no failures on the happy path, got %+v", stats.Failures)
	}
	if got := qc.get("shard-cpu"); got != 1 {
		t.Errorf("shard-cpu queries: got %d, want 1", got)
	}
	if got := qc.get("shard-mem"); got != 1 {
		t.Errorf("shard-mem queries: got %d, want 1", got)
	}
	if got := qc.get("shard-oom"); got != 1 {
		t.Errorf("shard-oom queries: got %d, want 1", got)
	}
	if got := qc.get("workload-cpu"); got != 0 {
		t.Errorf("workload-cpu (per-workload fallback) queries: got %d, want 0 -- batching must not degrade to per-workload", got)
	}
	if got := qc.get("workload-mem"); got != 0 {
		t.Errorf("workload-mem (per-workload fallback) queries: got %d, want 0 -- batching must not degrade to per-workload", got)
	}
}

// TestFetchWorkloadInputsBatch_PerIdentityIsolation verifies a single batched
// CPU response correctly attributes distinct per-workload values -- workload
// A's value must never leak into workload B's entry.
func TestFetchWorkloadInputsBatch_PerIdentityIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "workload_max_pod_cpu") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"0.5"]},
				{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"web","container":"app"},"value":[0,"1.5"]}
			]}}`))
			return
		}
		if strings.Contains(q, "workload_max_pod_memory") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"104857600"]},
				{"metric":{"namespace":"prod","owner_kind":"Deployment","owner_name":"web","container":"app"},"value":[0,"209715200"]}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	apiID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	cands := []promclient.ShardCandidate{
		{Identity: apiID, Containers: 1},
		{Identity: webID, Containers: 1},
	}

	out, _ := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	if got := out[apiID].CPUPerPod["app"]; got != 0.5 {
		t.Errorf("api CPU: got %v want 0.5", got)
	}
	if got := out[webID].CPUPerPod["app"]; got != 1.5 {
		t.Errorf("web CPU: got %v want 1.5 (must not bleed from api)", got)
	}
	if got := out[apiID].MemPerPod["app"]; got != 104857600 {
		t.Errorf("api mem: got %v want 104857600", got)
	}
	if got := out[webID].MemPerPod["app"]; got != 209715200 {
		t.Errorf("web mem: got %v want 209715200 (must not bleed from api)", got)
	}
}

// TestFetchWorkloadInputsBatch_ShardFailureFallsBackPerWorkload makes every
// shard query (CPU, memory, and OOM alike -- all of them use the
// `owner_name=~` alternation selector) fail, while per-workload
// (`owner_name="..."`, no tilde) queries succeed. It asserts the shard was
// retried exactly once before the code gave up on it, that per-workload
// queries were then issued for both names in the shard, and that both
// workloads still end up with usable data in the result -- the degrade path
// must not lose a healthy workload just because it shared a shard with a
// failure.
func TestFetchWorkloadInputsBatch_ShardFailureFallsBackPerWorkload(t *testing.T) {
	qc := newQueryCounter()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		cat := classifyQuery(q)
		qc.inc(cat)
		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(cat, "shard-") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"forced shard failure"}`))
			return
		}

		// Per-workload (fallback) query: succeed, with a value that encodes
		// which owner_name was requested so isolation can be checked too.
		m := ownerNameRE.FindStringSubmatch(q)
		name := ""
		if len(m) == 2 {
			name = m[1]
		}
		val := "0.1"
		if name == "web" {
			val = "0.2"
		}
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"), strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"container":"app"},"value":[0,"` + val + `"]}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	apiID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	cands := []promclient.ShardCandidate{
		{Identity: apiID, Containers: 1},
		{Identity: webID, Containers: 1},
	}

	out, stats := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	// The shard failed, but its per-workload fallback succeeded for both
	// names -- that must NOT be recorded as a BatchStats failure. Failures is
	// reserved for identities whose fallback ALSO failed (see
	// TestFetchWorkloadInputsBatch_TotalOutageStillYieldsPresentEntries).
	if len(stats.Failures) != 0 {
		t.Errorf("expected no failures when the fallback succeeds, got %+v", stats.Failures)
	}

	// Retried exactly once: 2 attempts (original + 1 retry), both failing,
	// before the fallback kicks in.
	if got := qc.get("shard-cpu"); got != 2 {
		t.Errorf("shard-cpu attempts: got %d, want 2 (1 original + 1 retry)", got)
	}
	if got := qc.get("shard-mem"); got != 2 {
		t.Errorf("shard-mem attempts: got %d, want 2 (1 original + 1 retry)", got)
	}

	// Per-workload fallback issued for both names in the shard.
	if got := qc.get("workload-cpu"); got != 2 {
		t.Errorf("workload-cpu fallback queries: got %d, want 2 (one per workload)", got)
	}
	if got := qc.get("workload-mem"); got != 2 {
		t.Errorf("workload-mem fallback queries: got %d, want 2 (one per workload)", got)
	}

	// Both workloads still present with usable data from the fallback.
	if out[apiID] == nil || out[apiID].CPUPerPod["app"] != 0.1 {
		t.Errorf("api: expected fallback CPU 0.1, got %+v", out[apiID])
	}
	if out[webID] == nil || out[webID].CPUPerPod["app"] != 0.2 {
		t.Errorf("web: expected fallback CPU 0.2, got %+v", out[webID])
	}
}

// TestFetchWorkloadInputsBatch_SparseOOMMap verifies that when only one
// workload in a shard has OOM samples, the other workload still gets a
// present, valid entry with a zero OOMSignal -- QueryShardOOMSignal's sparse
// map must never be mistaken for "missing" by the batch layer (see its doc
// comment in internal/prometheus/batch.go).
func TestFetchWorkloadInputsBatch_SparseOOMMap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "__name__") {
			// Only "api" has OOM history; "web" has none.
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"__name__":"k8s_sustain:workload_oom_24h","namespace":"prod","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"3"]}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	apiID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	cands := []promclient.ShardCandidate{
		{Identity: apiID, Containers: 1},
		{Identity: webID, Containers: 1},
	}

	out, _ := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	if out[apiID] == nil {
		t.Fatalf("api entry missing")
	}
	if got := out[apiID].OOM.OOMCounts["app"]; got != 3 {
		t.Errorf("api OOM count: got %v want 3", got)
	}
	web, ok := out[webID]
	if !ok || web == nil {
		t.Fatalf("web must have a present, non-nil entry even with no OOM data, got %v (present=%v)", web, ok)
	}
	if web.HasRecentOOM() {
		t.Errorf("web must have zero OOM signal, got %+v", web.OOM)
	}
}

// TestFetchWorkloadInputsBatch_EveryIdentityPresentEvenEmptyResponse verifies
// the map is pre-seeded from cands: with a Prometheus backend that returns
// no data for anything, every requested identity must still appear with
// non-nil (but empty) CPUPerPod/MemPerPod maps, so callers can distinguish
// "queried, nothing there" from "never queried".
func TestFetchWorkloadInputsBatch_EveryIdentityPresentEvenEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	cands := []promclient.ShardCandidate{
		{Identity: promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}, Containers: 1},
		{Identity: promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}, Containers: 1},
		{Identity: promclient.WorkloadIdentity{Namespace: "staging", OwnerKind: "StatefulSet", OwnerName: "db"}, Containers: 2},
	}

	out, stats := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(out), out)
	}
	if len(stats.Failures) != 0 {
		t.Errorf("a successful-but-empty response must not be recorded as a failure, got %+v", stats.Failures)
	}
	for _, c := range cands {
		wi, ok := out[c.Identity]
		if !ok || wi == nil {
			t.Fatalf("identity %+v missing or nil in result", c.Identity)
		}
		if wi.CPUPerPod == nil {
			t.Errorf("identity %+v: CPUPerPod must be non-nil", c.Identity)
		}
		if wi.MemPerPod == nil {
			t.Errorf("identity %+v: MemPerPod must be non-nil", c.Identity)
		}
		if len(wi.CPUPerPod) != 0 || len(wi.MemPerPod) != 0 {
			t.Errorf("identity %+v: expected empty maps, got %+v", c.Identity, wi)
		}
		if wi.HasRecentOOM() {
			t.Errorf("identity %+v: expected zero OOM signal, got %+v", c.Identity, wi.OOM)
		}
	}
}

// TestFetchWorkloadInputsBatch_TotalOutageStillYieldsPresentEntries is the
// third degradation tier the whole design is built around, and the one most
// likely to be silently broken by a future refactor: EVERY query -- shard
// queries (both attempts) AND the per-workload fallback queries they trigger
// -- fails. Per FetchWorkloadInputsBatch's contract, that must not panic, must
// not lose any identity from the result, and must leave every requested
// identity with a present, non-nil BatchInputs entry carrying the pre-seeded
// empty CPUPerPod/MemPerPod maps -- the recommendation math degrades exactly
// as if the identity had been queried and found to have no data.
//
// UNLIKE that recommendation-math view, this is exactly the case BatchStats
// exists to distinguish: both identities' fallback fetches genuinely failed,
// so both MUST appear in stats.Failures with their underlying error -- a
// caller consulting only BatchInputs would otherwise see this total outage as
// indistinguishable from two workloads that simply have no data yet.
func TestFetchWorkloadInputsBatch_TotalOutageStillYieldsPresentEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"forced total outage"}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	apiID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"}
	webID := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "web"}
	cands := []promclient.ShardCandidate{
		{Identity: apiID, Containers: 1},
		{Identity: webID, Containers: 1},
	}

	out, stats := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	if len(out) != 2 {
		t.Fatalf("expected 2 entries even under total outage, got %d: %+v", len(out), out)
	}
	for _, id := range []promclient.WorkloadIdentity{apiID, webID} {
		wi, ok := out[id]
		if !ok || wi == nil {
			t.Fatalf("identity %+v missing or nil under total outage", id)
		}
		if wi.CPUPerPod == nil || wi.MemPerPod == nil {
			t.Errorf("identity %+v: CPUPerPod/MemPerPod must stay non-nil under total outage, got %+v", id, wi)
		}
		if len(wi.CPUPerPod) != 0 || len(wi.MemPerPod) != 0 {
			t.Errorf("identity %+v: expected empty maps under total outage, got %+v", id, wi)
		}
		if wi.HasRecentOOM() {
			t.Errorf("identity %+v: expected zero OOM signal under total outage, got %+v", id, wi.OOM)
		}
		if stats.Failures[id] == nil {
			t.Errorf("identity %+v: expected a recorded failure under total outage, got none (Failures=%+v)", id, stats.Failures)
		}
	}
}

// A shard is large by design -- at the shipped --query-shard-max-samples a 7d
// single-container shard holds close to a thousand workloads -- and the failure
// that drives fallbackShardPerWorkload (Prometheus rejecting an over-budget
// query) is deterministic, so the retry above it fails identically and the
// fallback is the normal path rather than a rare one. Walking those names
// serially would mean a thousand sequential round trips in one goroutine; with
// a slow-but-alive Prometheus (never slow enough to trip the breaker) that can
// outlast --reconcile-interval and the 10m sweepGracePeriod that
// recommendation_cache.go documents as a correctness assumption.
//
// So this pins the property that the fallback overlaps its queries. It asserts
// only that concurrency is >1 and never exceeds the bound: pinning an exact
// value would make the test a change-detector, and the useful invariants are
// "not serial" and "bounded".
//
// It counts concurrently in-flight WORKLOADS, not in-flight requests. That
// distinction is the whole test: FetchWorkloadInputs fans its own CPU/memory/
// OOM sub-queries out via errgroup, so even a strictly serial fallback shows
// about three requests in flight at once. Only the number of distinct
// owner_names being served simultaneously separates "one workload at a time"
// from "fallbackFetchConcurrency workloads at a time" -- an earlier version of
// this test counted raw requests and passed against a serial implementation.
func TestFallbackShardPerWorkload_QueriesOverlapWithinBound(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight = map[string]int{}
		peak     int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// Per-workload fallback queries use an exact owner_name="..." matcher;
		// the shard queries that must fail to drive the fallback in the first
		// place use owner_name=~"a|b" and so do not match here.
		m := ownerNameRE.FindStringSubmatch(r.Form.Get("query"))
		if m == nil {
			// A shard query: fail it, which is what hands this shard to
			// fallbackShardPerWorkload. Not counted.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"forced shard failure"}`))
			return
		}

		mu.Lock()
		inFlight[m[1]]++
		if len(inFlight) > peak {
			peak = len(inFlight)
		}
		mu.Unlock()

		// Long enough that a serial implementation cannot overlap by accident,
		// short enough to keep the test quick.
		time.Sleep(20 * time.Millisecond)

		// Both details below are load-bearing: the bound is enforced
		// client-side but measured here, server-side.
		//
		//   - The fallback queries must SUCCEED. A failure cancels its
		//     errgroup siblings, so the client frees its slot and starts the
		//     next workload while the abandoned handlers are still counted
		//     here — a measurement artifact that shows up as spurious peaks.
		//   - The decrement must happen BEFORE the response is written, so
		//     this owner is uncounted by the time the client can admit
		//     another. A deferred decrement loses that race under load.
		mu.Lock()
		if inFlight[m[1]]--; inFlight[m[1]] <= 0 {
			delete(inFlight, m[1])
		}
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	// The client's own in-flight semaphore is the authoritative Prometheus
	// throttle and would otherwise mask the goroutine bound under test.
	pc, err := promclient.New(server.URL, promclient.WithMaxInflight(64))
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	const numNames = 24
	cands := make([]promclient.ShardCandidate, 0, numNames)
	for i := range numNames {
		cands = append(cands, promclient.ShardCandidate{
			Identity:   promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: fmt.Sprintf("app-%02d", i)},
			Containers: 1,
		})
	}

	// One shard, so every name lands in a single fallbackShardPerWorkload call.
	FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000_000)

	mu.Lock()
	got := peak
	mu.Unlock()

	if got < 2 {
		t.Errorf("peak concurrently-queried workloads was %d: the fallback is running serially, "+
			"which on a full-size shard is ~1000 sequential round trips in one goroutine", got)
	}
	if got > fallbackFetchConcurrency {
		t.Errorf("peak concurrently-queried workloads was %d, above fallbackFetchConcurrency=%d",
			got, fallbackFetchConcurrency)
	}
}

// TestFetchWorkloadInputsBatch_FailureErrorSurvivesCircuitOpen verifies that
// once a sustained outage trips the underlying Client's circuit breaker (5
// consecutive genuine failures -- see internal/prometheus/client.go's
// defaultBreakerMaxFailures), the resulting promclient.ErrCircuitOpen is
// still detectable via errors.Is on the error BatchStats.Failures records --
// neither this package nor fallbackShardPerWorkload's FetchWorkloadInputs
// call may wrap it away. This is the strongest available "Prometheus is
// known down" signal, and losing it would leave callers unable to
// distinguish that from an ordinary, more transient query error.
//
// A large-enough name count is used deliberately: FetchWorkloadInputs runs
// its CPU/memory/OOM sub-queries concurrently, and an errgroup sibling
// failure cancels its siblings' in-flight queries as collateral damage,
// which recordQueryFailure deliberately does NOT count as a breaker failure
// (see its doc comment) -- so a single fallback name reliably contributes
// only about one counted failure, not three.
//
// fallbackShardPerWorkload dispatches those names in waves of at most
// fallbackFetchConcurrency, so what makes this deterministic is the ratio, not
// serial execution: with numNames comfortably above
// fallbackFetchConcurrency + defaultBreakerMaxFailures, the earlier waves
// contribute more than enough counted failures to trip the breaker before the
// final wave calls breaker.allow(), and the 30s cooldown cannot elapse inside
// the test. Every name in that last wave therefore observes an open breaker.
// (Keep numNames above that sum if either constant is ever retuned.)
func TestFetchWorkloadInputsBatch_FailureErrorSurvivesCircuitOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"forced total outage"}`))
	}))
	defer server.Close()

	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	const numNames = 20
	cands := make([]promclient.ShardCandidate, 0, numNames)
	ids := make([]promclient.WorkloadIdentity, 0, numNames)
	for i := 0; i < numNames; i++ {
		id := promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: fmt.Sprintf("app-%02d", i)}
		ids = append(ids, id)
		cands = append(cands, promclient.ShardCandidate{Identity: id, Containers: 1})
	}

	_, stats := FetchWorkloadInputsBatch(context.Background(), pc, cands, batchTestRsCfg(), 1_000_000)

	sawCircuitOpen := false
	for _, id := range ids {
		if errors.Is(stats.Failures[id], promclient.ErrCircuitOpen) {
			sawCircuitOpen = true
			break
		}
	}
	if !sawCircuitOpen {
		t.Errorf("expected at least one recorded failure to be (or wrap) promclient.ErrCircuitOpen once the breaker trips, got %+v", stats.Failures)
	}
}
