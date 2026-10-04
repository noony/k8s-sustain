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

// shardQuery is one PromQL request the fake Prometheus received: which signal
// it read and which owner names its alternation selected.
type shardQuery struct {
	signal string
	names  []string
}

var ownerAlternationRE = regexp.MustCompile(`owner_name=~"([^"]*)"`)

func parseShardQuery(q string) shardQuery {
	var sq shardQuery
	switch {
	case strings.Contains(q, "__name__"):
		sq.signal = "oom"
	case strings.Contains(q, "workload_max_pod_cpu"):
		sq.signal = "cpu"
	case strings.Contains(q, "workload_max_pod_memory"):
		sq.signal = "memory"
	default:
		sq.signal = "unknown: " + q
	}
	if m := ownerAlternationRE.FindStringSubmatch(q); m != nil {
		sq.names = strings.Split(m[1], "|")
	}
	return sq
}

// fakeShardProm is a Prometheus that answers shard queries through respond and
// records every query it received. respond returns the vector's samples, or
// ok=false to fail the query with a 500.
type fakeShardProm struct {
	mu      sync.Mutex
	queries []shardQuery
}

func startFakeShardProm(t *testing.T, respond func(shardQuery) (samples []string, ok bool), opts ...promclient.Option) (*fakeShardProm, *promclient.Client) {
	t.Helper()
	f := &fakeShardProm{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := parseShardQuery(r.Form.Get("query"))
		f.mu.Lock()
		f.queries = append(f.queries, q)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		samples, ok := respond(q)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"internal","error":"forced failure"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(samples, ","))
	}))
	t.Cleanup(server.Close)
	c, err := promclient.New(server.URL, opts...)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}
	return f, c
}

// count returns how many signal queries selected exactly names owner names.
func (f *fakeShardProm) count(signal string, names int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if q.signal == signal && len(q.names) == names {
			n++
		}
	}
	return n
}

// shardsFor returns the name sets of every signal query, in arrival order.
func (f *fakeShardProm) shardsFor(signal string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, q := range f.queries {
		if q.signal == signal {
			out = append(out, q.names)
		}
	}
	return out
}

// sample renders one vector sample for identity prod/Deployment/owner. name is
// the series' __name__, which only the OOM fold reads.
func sample(name, owner, container string, v float64) string {
	return fmt.Sprintf(`{"metric":{"__name__":%q,"namespace":"prod","owner_kind":"Deployment","owner_name":%q,"container":%q},"value":[0,"%g"]}`,
		name, owner, container, v)
}

func prodID(name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: name}
}

func reqsFor(containers int, names ...string) []InputsRequest {
	out := make([]InputsRequest, 0, len(names))
	for _, n := range names {
		out = append(out, InputsRequest{Identity: prodID(n), Containers: containers})
	}
	return out
}

// promInputsCfg uses a one-hour window on both resources, so a one-container
// identity costs 60 samples against the shard budget.
func promInputsCfg() sustainv1alpha1.ResourcesConfigs {
	p95 := int32(95)
	return sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{Window: "1h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
		Memory: sustainv1alpha1.ResourceConfig{Window: "1h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
	}
}

func emptyAnswer(shardQuery) ([]string, bool) { return nil, true }

// Two identities in one namespace and kind must cost one query per signal, not
// one per identity, and each value must land on its own identity. An identity
// with no OOM series still gets inputs, with an empty OOM signal.
func TestPromInputs_BatchesIdentitiesIntoOneShardPerSignal(t *testing.T) {
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		switch q.signal {
		case "cpu":
			return []string{sample("cpu", "api", "app", 0.5), sample("cpu", "web", "app", 1.5)}, true
		case "memory":
			return []string{sample("mem", "api", "app", 100), sample("mem", "web", "app", 200)}, true
		default:
			return []string{sample("k8s_sustain:workload_oom_24h", "api", "app", 3)}, true
		}
	})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api", "web"))

	for _, signal := range []string{"cpu", "memory", "oom"} {
		if n := prom.count(signal, 2); n != 1 {
			t.Errorf("%s: %d two-identity queries, want exactly 1 (shards: %v)", signal, n, prom.shardsFor(signal))
		}
		if n := len(prom.shardsFor(signal)); n != 1 {
			t.Errorf("%s: %d queries in total, want 1", signal, n)
		}
	}
	api, web := got[prodID("api")], got[prodID("web")]
	if api.Err != nil || web.Err != nil {
		t.Fatalf("unexpected errors: api=%v web=%v", api.Err, web.Err)
	}
	if api.Inputs.CPUPerPod["app"] != 0.5 || web.Inputs.CPUPerPod["app"] != 1.5 {
		t.Errorf("cpu = %v / %v, want 0.5 / 1.5", api.Inputs.CPUPerPod, web.Inputs.CPUPerPod)
	}
	if api.Inputs.MemPerPod["app"] != 100 || web.Inputs.MemPerPod["app"] != 200 {
		t.Errorf("memory = %v / %v, want 100 / 200", api.Inputs.MemPerPod, web.Inputs.MemPerPod)
	}
	if api.Inputs.OOM.OOMCounts["app"] != 3 {
		t.Errorf("api OOM count = %v, want 3", api.Inputs.OOM.OOMCounts["app"])
	}
	if web.Inputs.HasRecentOOM() {
		t.Errorf("web has no OOM series, got %+v", web.Inputs.OOM)
	}
}

// The sample budget, not the namespace, decides how many identities share a
// query: at 60 samples each, a 100-sample budget fits one identity per shard.
func TestPromInputs_SplitsShardsAtTheSampleBudget(t *testing.T) {
	prom, c := startFakeShardProm(t, emptyAnswer)

	NewPromInputs(c, 100).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api", "web"))

	if n := prom.count("cpu", 1); n != 2 {
		t.Errorf("cpu: %d one-identity queries, want 2 (shards: %v)", n, prom.shardsFor("cpu"))
	}
}

// An identity of unknown size cannot be costed, so it never shares a shard.
func TestPromInputs_UnknownSizeIdentityGetsItsOwnShard(t *testing.T) {
	prom, c := startFakeShardProm(t, emptyAnswer)
	reqs := []InputsRequest{
		{Identity: prodID("api"), Containers: 2},
		{Identity: prodID("unsized")},
		{Identity: prodID("web"), Containers: 1},
	}

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqs)

	shards := prom.shardsFor("cpu")
	if len(shards) != 2 {
		t.Fatalf("cpu shards = %v, want [api web] and [unsized]", shards)
	}
	for _, s := range shards {
		if len(s) == 2 && strings.Join(s, ",") != "api,web" {
			t.Errorf("sized shard = %v, want [api web]", s)
		}
		if len(s) == 1 && s[0] != "unsized" {
			t.Errorf("one-identity shard = %v, want [unsized]", s)
		}
	}
	if r := got[prodID("unsized")]; r.Err != nil || r.Inputs == nil {
		t.Errorf("unsized identity = %+v, want empty inputs", r)
	}
}

// A shard query that fails once and then succeeds is a transient blip: one
// retry, no fallback.
func TestPromInputs_RetriesAFailedShardOnce(t *testing.T) {
	var mu sync.Mutex
	failed := map[string]bool{}
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !failed[q.signal] {
			failed[q.signal] = true
			return nil, false
		}
		if q.signal == "cpu" {
			return []string{sample("cpu", "api", "app", 0.5)}, true
		}
		return nil, true
	})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api", "web"))

	if r := got[prodID("api")]; r.Err != nil || r.Inputs.CPUPerPod["app"] != 0.5 {
		t.Errorf("api = %+v, want the retried shard's 0.5", r)
	}
	for _, signal := range []string{"cpu", "memory", "oom"} {
		if n := prom.count(signal, 2); n != 2 {
			t.Errorf("%s: %d attempts on the shard, want 2 (one retry)", signal, n)
		}
		if n := prom.count(signal, 1); n != 0 {
			t.Errorf("%s: %d one-identity queries, want 0: a retry that succeeds needs no fallback", signal, n)
		}
	}
}

// A shard that keeps failing is re-queried one identity at a time, so its
// members still get their inputs. This is the over-budget case: Prometheus
// rejects the packed query deterministically but answers each identity alone.
func TestPromInputs_FailedShardFallsBackToOneIdentityShards(t *testing.T) {
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		if len(q.names) > 1 {
			return nil, false
		}
		v := 0.1
		if q.names[0] == "web" {
			v = 0.2
		}
		if q.signal == "oom" {
			return nil, true
		}
		return []string{sample(q.signal, q.names[0], "app", v)}, true
	})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api", "web"))

	for _, signal := range []string{"cpu", "memory", "oom"} {
		if n := prom.count(signal, 2); n != 2 {
			t.Errorf("%s: %d attempts on the shard, want 2", signal, n)
		}
		if n := prom.count(signal, 1); n != 2 {
			t.Errorf("%s: %d one-identity queries, want 2 (one per identity)", signal, n)
		}
	}
	if r := got[prodID("api")]; r.Err != nil || r.Inputs.CPUPerPod["app"] != 0.1 {
		t.Errorf("api = %+v, want CPU 0.1 from its own query", r)
	}
	if r := got[prodID("web")]; r.Err != nil || r.Inputs.MemPerPod["app"] != 0.2 {
		t.Errorf("web = %+v, want memory 0.2 from its own query", r)
	}
}

// CPU and memory are the recommendation's primary inputs: an identity whose
// query fails even alone gets an error, and its healthy shard-mate is
// unaffected.
func TestPromInputs_RequiredSignalFailureIsThatIdentitysError(t *testing.T) {
	for _, signal := range []string{"cpu", "memory"} {
		t.Run(signal, func(t *testing.T) {
			_, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
				if q.signal == signal && (len(q.names) > 1 || q.names[0] == "bad") {
					return nil, false
				}
				if q.signal == "oom" {
					return nil, true
				}
				return []string{sample(q.signal, "good", "app", 1)}, true
			})

			got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "bad", "good"))

			if r := got[prodID("bad")]; r.Err == nil || r.Inputs != nil {
				t.Errorf("bad = %+v, want an error and no inputs", r)
			} else if !strings.Contains(r.Err.Error(), signal) {
				t.Errorf("bad error = %v, want it to name the %s signal", r.Err, signal)
			}
			if r := got[prodID("good")]; r.Err != nil || r.Inputs == nil {
				t.Errorf("good = %+v, want inputs despite its shard-mate failing", r)
			}
		})
	}
}

// OOM only raises a memory floor, so its outage must not cost an identity its
// recommendation: the inputs come back with an empty OOM signal.
func TestPromInputs_OOMFailureIsBestEffort(t *testing.T) {
	_, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		switch q.signal {
		case "oom":
			return nil, false
		case "cpu":
			return []string{sample("cpu", "api", "app", 0.5)}, true
		default:
			return nil, true
		}
	})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api"))

	r := got[prodID("api")]
	if r.Err != nil {
		t.Fatalf("an OOM failure must not fail the identity, got %v", r.Err)
	}
	if r.Inputs.CPUPerPod["app"] != 0.5 {
		t.Errorf("cpu = %v, want 0.5", r.Inputs.CPUPerPod)
	}
	if r.Inputs.HasRecentOOM() {
		t.Errorf("OOM = %+v, want empty", r.Inputs.OOM)
	}
}

// "Queried and found nothing" must stay distinguishable from "could not
// query": every identity gets non-nil, empty maps and no error.
func TestPromInputs_EmptyResponseYieldsEmptyInputs(t *testing.T) {
	_, c := startFakeShardProm(t, emptyAnswer)
	reqs := append(reqsFor(1, "api", "web"),
		InputsRequest{Identity: promclient.WorkloadIdentity{Namespace: "staging", OwnerKind: "StatefulSet", OwnerName: "db"}, Containers: 2})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqs)

	if len(got) != len(reqs) {
		t.Fatalf("got %d results, want one per request (%d)", len(got), len(reqs))
	}
	for _, r := range reqs {
		res := got[r.Identity]
		if res.Err != nil || res.Inputs == nil {
			t.Fatalf("%v = %+v, want empty inputs and no error", r.Identity, res)
		}
		if res.Inputs.CPUPerPod == nil || res.Inputs.MemPerPod == nil {
			t.Errorf("%v: CPUPerPod and MemPerPod must be non-nil", r.Identity)
		}
		if len(res.Inputs.CPUPerPod) != 0 || len(res.Inputs.MemPerPod) != 0 || res.Inputs.HasRecentOOM() {
			t.Errorf("%v: want empty inputs, got %+v", r.Identity, res.Inputs)
		}
	}
}

// A shard holds up to a thousand identities and the over-budget failure that
// drives the fallback is deterministic, so the fallback is a normal path. Its
// one-identity queries must overlap, or a full shard is a thousand sequential
// round trips, and stay within fallbackFetchConcurrency.
//
// It measures distinct identities in flight, decremented before the response
// is written so the client cannot admit the next one while this one still
// counts. Fallback queries succeed: a failure would be retried and blur the
// measurement.
func TestPromInputs_FallbackOverlapsWithinBound(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight = map[string]int{}
		peak     int
	)
	_, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		if len(q.names) != 1 {
			return nil, false
		}
		name := q.signal + "/" + q.names[0]
		mu.Lock()
		inFlight[name]++
		peak = max(peak, len(inFlight))
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		if inFlight[name]--; inFlight[name] <= 0 {
			delete(inFlight, name)
		}
		mu.Unlock()
		return nil, true
	}, promclient.WithMaxInflight(64))

	names := make([]string, 24)
	for i := range names {
		names[i] = fmt.Sprintf("app-%02d", i)
	}
	NewPromInputs(c, 1_000_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, names...))

	mu.Lock()
	got := peak
	mu.Unlock()
	if got < 2 {
		t.Errorf("peak concurrent one-identity queries = %d: the fallback runs serially", got)
	}
	if got > fallbackFetchConcurrency {
		t.Errorf("peak concurrent one-identity queries = %d, above fallbackFetchConcurrency=%d", got, fallbackFetchConcurrency)
	}
}

// On a cancelled context every identity reports the cancellation: a fetch cut
// short must not read as "no samples".
func TestPromInputs_CancelledContextFailsEveryIdentity(t *testing.T) {
	_, c := startFakeShardProm(t, emptyAnswer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := NewPromInputs(c, 1_000_000).FetchInputs(ctx, promInputsCfg(), reqsFor(1, "api", "web"))

	for _, n := range []string{"api", "web"} {
		if r := got[prodID(n)]; !errors.Is(r.Err, context.Canceled) {
			t.Errorf("%s = %+v, want a context.Canceled error", n, r)
		}
	}
}

// Once a sustained outage trips the client's breaker, ErrCircuitOpen is the
// strongest "Prometheus is down" signal; the identity's error must still match
// it with errors.Is. Twenty identities fail well past the breaker's five
// counted failures before the last fallback wave asks it for a slot.
func TestPromInputs_CircuitOpenSurvivesInTheError(t *testing.T) {
	_, c := startFakeShardProm(t, func(shardQuery) ([]string, bool) { return nil, false })
	names := make([]string, 20)
	for i := range names {
		names[i] = fmt.Sprintf("app-%02d", i)
	}

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, names...))

	for _, n := range names {
		if errors.Is(got[prodID(n)].Err, promclient.ErrCircuitOpen) {
			return
		}
	}
	t.Errorf("no identity's error matches promclient.ErrCircuitOpen: %+v", got)
}
