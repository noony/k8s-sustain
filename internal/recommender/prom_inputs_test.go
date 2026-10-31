package recommender

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// shardQuery is one PromQL request the fake Prometheus received: its
// expression, which signal it read, which owner names its alternation
// selected, and the instant it was evaluated as of or, for a range query, its
// start, end and step.
type shardQuery struct {
	expr             string
	signal           string
	names            []string
	at               string
	start, end, step string
}

var ownerAlternationRE = regexp.MustCompile(`owner_name=~"([^"]*)"`)

func parseShardQuery(q string) shardQuery {
	sq := shardQuery{expr: q}
	switch {
	case strings.Contains(q, promclient.MetricContainerOOMKills):
		sq.signal = "kills"
	case strings.Contains(q, "__name__"):
		sq.signal = "oom"
	case strings.Contains(q, "workload_max_pod_cpu"):
		sq.signal = "cpu"
	case strings.Contains(q, "workload_max_pod_memory"):
		sq.signal = "memory"
	case strings.Contains(q, listedSignalRule):
		sq.signal = "listed"
	default:
		sq.signal = "unknown: " + q
	}
	if m := ownerAlternationRE.FindStringSubmatch(q); m != nil {
		sq.names = strings.Split(m[1], "|")
	}
	return sq
}

// fakeShardProm is a Prometheus that answers shard queries through respond and
// records every query it received. respond returns the vector's samples, or a
// range query's matrix's series, or ok=false to fail the query with a 500.
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
		q.at, q.start, q.end, q.step = r.Form.Get("time"), r.Form.Get("start"), r.Form.Get("end"), r.Form.Get("step")
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
		resultType := "vector"
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			resultType = "matrix"
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":[%s]}}`, resultType, strings.Join(samples, ","))
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

// series renders one matrix series for identity prod/Deployment/owner, valued
// at each instant of values.
func series(name, owner, container string, values map[time.Time]float64) string {
	var pts []string
	for _, at := range slices.SortedFunc(maps.Keys(values), time.Time.Compare) {
		pts = append(pts, fmt.Sprintf(`[%d,"%g"]`, at.Unix(), values[at]))
	}
	return fmt.Sprintf(`{"metric":{"__name__":%q,"namespace":"prod","owner_kind":"Deployment","owner_name":%q,"container":%q},"values":[%s]}`,
		name, owner, container, strings.Join(pts, ","))
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
// one per identity, each evaluated as of the instant fetched as of, and each
// value must land on its own identity. An identity with no OOM series still
// gets inputs, with an empty OOM signal.
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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "api", "web"))

	for _, signal := range []string{"cpu", "memory", "oom"} {
		if n := prom.count(signal, 2); n != 1 {
			t.Errorf("%s: %d two-identity queries, want exactly 1 (shards: %v)", signal, n, prom.shardsFor(signal))
		}
		if n := len(prom.shardsFor(signal)); n != 1 {
			t.Errorf("%s: %d queries in total, want 1", signal, n)
		}
	}
	for _, q := range prom.queries {
		if want := strconv.FormatInt(now.Unix(), 10); q.at != want {
			t.Errorf("%s evaluated as of %q, want %q", q.signal, q.at, want)
		}
	}
	api, web := got[prodID("api")], got[prodID("web")]
	if api.Err != nil || web.Err != nil {
		t.Fatalf("unexpected errors: api=%v web=%v", api.Err, web.Err)
	}
	wantAPI := NewInputs(
		WithCPUUsage(promclient.ContainerValues{"app": 0.5}),
		WithMemoryUsage(promclient.ContainerValues{"app": 100}),
		WithOOMKills(map[string]OOM{"app": {Kills: 3}}),
	)
	if !reflect.DeepEqual(api.Inputs, wantAPI) {
		t.Errorf("api = %+v, want %+v", api.Inputs, wantAPI)
	}
	wantWeb := NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 1.5}), WithMemoryUsage(promclient.ContainerValues{"app": 200}))
	if !reflect.DeepEqual(web.Inputs, wantWeb) {
		t.Errorf("web = %+v, want %+v without OOM kills", web.Inputs, wantWeb)
	}
}

// The sample budget, not the namespace, decides how many identities share a
// query: at 60 samples each, a 100-sample budget fits one identity per shard.
func TestPromInputs_SplitsShardsAtTheSampleBudget(t *testing.T) {
	prom, c := startFakeShardProm(t, emptyAnswer)

	NewPromInputs(c, 100).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "api", "web"))

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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqs)

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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "api", "web"))

	if r := got[prodID("api")]; r.Err != nil || r.Inputs.Usage("app", corev1.ResourceCPU) != 0.5 {
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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "api", "web"))

	for _, signal := range []string{"cpu", "memory", "oom"} {
		if n := prom.count(signal, 2); n != 2 {
			t.Errorf("%s: %d attempts on the shard, want 2", signal, n)
		}
		if n := prom.count(signal, 1); n != 2 {
			t.Errorf("%s: %d one-identity queries, want 2 (one per identity)", signal, n)
		}
	}
	if r := got[prodID("api")]; r.Err != nil || r.Inputs.Usage("app", corev1.ResourceCPU) != 0.1 {
		t.Errorf("api = %+v, want CPU 0.1 from its own query", r)
	}
	if r := got[prodID("web")]; r.Err != nil || r.Inputs.Usage("app", corev1.ResourceMemory) != 0.2 {
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

			got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "bad", "good"))

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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, "api"))

	r := got[prodID("api")]
	if r.Err != nil {
		t.Fatalf("an OOM failure must not fail the identity, got %v", r.Err)
	}
	if want := NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.5})); !reflect.DeepEqual(r.Inputs, want) {
		t.Errorf("inputs = %+v, want %+v with no OOM kills", r.Inputs, want)
	}
}

// "Queried and found nothing" must stay distinguishable from "could not
// query": every identity gets inputs and no error. They hold every signal's
// values from no samples, exactly the inputs NewInputs builds with none set,
// which is what an InputsFetcher serving canned inputs answers for an
// identity it knows nothing of.
func TestPromInputs_EmptyResponseYieldsEmptyInputs(t *testing.T) {
	_, c := startFakeShardProm(t, emptyAnswer)
	reqs := append(reqsFor(1, "api", "web"),
		InputsRequest{Identity: promclient.WorkloadIdentity{Namespace: "staging", OwnerKind: "StatefulSet", OwnerName: "db"}, Containers: 2})

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqs)

	if len(got) != len(reqs) {
		t.Fatalf("got %d results, want one per request (%d)", len(got), len(reqs))
	}
	for _, r := range reqs {
		if res := got[r.Identity]; res.Err != nil || !reflect.DeepEqual(res.Inputs, NewInputs()) {
			t.Errorf("%v = %+v, want empty inputs and no error", r.Identity, res)
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
	NewPromInputs(c, 1_000_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, names...))

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

	got := NewPromInputs(c, 1_000_000).FetchInputs(ctx, now, promInputsCfg(), reqsFor(1, "api", "web"))

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

	got := NewPromInputs(c, 1_000_000).FetchInputs(context.Background(), now, promInputsCfg(), reqsFor(1, names...))

	for _, n := range names {
		if errors.Is(got[prodID(n)].Err, promclient.ErrCircuitOpen) {
			return
		}
	}
	t.Errorf("no identity's error matches promclient.ErrCircuitOpen: %+v", got)
}

// A history is each signal's own shard query, the one an instant read runs,
// run over the span as a range query, in shards planned on its samples at
// every instant: at 60 samples over three instants, a 300-sample budget that
// packs both identities into one instant query splits them. Each instant's
// values land on their own identity, and the OOM signal's history query,
// run in the same shard, counts the kills in the step they fell in.
func TestPromInputs_FetchHistoryRunsEachSignalsQueryOverTheSpan(t *testing.T) {
	span := SpanOf(now.Add(-2*time.Minute), now, time.Minute)
	t0, t1, t2 := span.Start, span.Start.Add(time.Minute), now
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		switch {
		case q.signal == "cpu" && slices.Equal(q.names, []string{"api"}):
			return []string{series("", "api", "app", map[time.Time]float64{t0: 0.1, t1: 0.2, t2: 0.3})}, true
		case q.signal == "cpu":
			return []string{series("", "web", "app", map[time.Time]float64{t2: 1.5})}, true
		case q.signal == "memory" && slices.Equal(q.names, []string{"api"}):
			return []string{series("", "api", "app", map[time.Time]float64{t0: 100, t1: 100, t2: 100})}, true
		case q.signal == "oom":
			return []string{series(promclient.MetricWorkloadOOM24h, "api", "app", map[time.Time]float64{t2: 1})}, true
		case q.signal == "kills":
			return []string{series(promclient.MetricContainerOOMKills, "api", "app", map[time.Time]float64{t1: 0, t2: 2})}, true
		}
		return nil, true
	})
	cfg := promInputsCfg()

	got := NewPromInputs(c, 300).FetchHistory(context.Background(), span, cfg, reqsFor(1, "api", "web"))

	if n := prom.count("cpu", 1); n != 2 {
		t.Errorf("cpu: %d one-identity queries, want 2 (shards: %v)", n, prom.shardsFor("cpu"))
	}
	start, end := strconv.FormatInt(t0.Unix(), 10), strconv.FormatInt(t2.Unix(), 10)
	for _, q := range prom.queries {
		if q.start != start || q.end != end || q.step != "60" {
			t.Errorf("%s ran over %s..%s every %s, want %s..%s every 60", q.signal, q.start, q.end, q.step, start, end)
		}
		single := promclient.Shard{Namespace: "prod", OwnerKind: "Deployment", Names: q.names}
		if q.signal == "cpu" && q.expr != cpuUsage.query(cfg, single) {
			t.Errorf("cpu ran %q, want the instant read's own query %q", q.expr, cpuUsage.query(cfg, single))
		}
		if q.signal == "kills" && q.expr != (oomFloor{}).historyQuery(single, span.Step) {
			t.Errorf("kills ran %q, want the OOM signal's history query %q", q.expr, (oomFloor{}).historyQuery(single, span.Step))
		}
	}
	if kills, oom := prom.shardsFor("kills"), prom.shardsFor("oom"); !reflect.DeepEqual(kills, oom) {
		t.Errorf("kills read in shards %v, want the OOM signal's %v", kills, oom)
	}
	empty := func(at time.Time) HistoryPoint { return HistoryPoint{At: at, Inputs: NewInputs()} }
	wantAPI := NewHistory([]HistoryPoint{
		{At: t0, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.1}), WithMemoryUsage(promclient.ContainerValues{"app": 100}))},
		{At: t1, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.2}), WithMemoryUsage(promclient.ContainerValues{"app": 100}),
			WithOOMKills(map[string]OOM{"app": {}}))},
		{At: t2, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.3}), WithMemoryUsage(promclient.ContainerValues{"app": 100}),
			WithOOMKills(map[string]OOM{"app": {Kills: 1, KillsInStep: 2}}))},
	})
	if r := got[prodID("api")]; r.Err != nil || !reflect.DeepEqual(r.History, wantAPI) {
		t.Errorf("api = %+v (%v), want %+v", r.History, r.Err, wantAPI)
	}
	wantWeb := NewHistory([]HistoryPoint{
		empty(t0), empty(t1),
		{At: t2, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 1.5}))},
	})
	if r := got[prodID("web")]; r.Err != nil || !reflect.DeepEqual(r.History, wantWeb) {
		t.Errorf("web = %+v (%v), want %+v", r.History, r.Err, wantWeb)
	}
}

// A history fails as inputs do: an identity whose required signal cannot be
// read even on its own has no history, and one whose best-effort signal
// cannot be read, its history query included, holds none of its values.
func TestPromInputs_FetchHistoryFailsAsInputsDo(t *testing.T) {
	span := SpanOf(now.Add(-time.Minute), now, time.Minute)
	_, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		switch q.signal {
		case "cpu":
			return []string{series("", "good", "app", map[time.Time]float64{now: 0.5})}, !slices.Contains(q.names, "bad")
		case "kills":
			return nil, false
		}
		return nil, true
	})

	got := NewPromInputs(c, 1_000_000).FetchHistory(context.Background(), span, promInputsCfg(), reqsFor(1, "bad", "good"))

	if r := got[prodID("bad")]; r.Err == nil || r.History != nil {
		t.Errorf("bad = %+v, want its cpu error", r)
	}
	want := NewHistory([]HistoryPoint{
		{At: span.Start, Inputs: NewInputs()},
		{At: now, Inputs: NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.5}))},
	})
	if r := got[prodID("good")]; r.Err != nil || !reflect.DeepEqual(r.History, want) {
		t.Errorf("good = %+v (%v), want its history without OOM values", r.History, r.Err)
	}
}
