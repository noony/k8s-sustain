package recommender

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const listedSignalRule = "k8s_sustain:listed_signal"

// listedSignal is a signal that exists only in this file: a required memory
// limit adjuster costing ten samples per container, raising a container's
// memory limit to factor times the limit its rule says the container was
// killed at. Its values are of its own type, kept in WorkloadInputs under the
// signal like any other's.
type listedSignal struct{ factor float64 }

// listedValues are listedSignal's values: the limit each container was killed
// at, by its rule.
type listedValues map[string]float64

func (listedValues) observed() []string { return nil }
func (listedValues) excusesAge() bool   { return false }

func (listedSignal) name() string   { return "listed" }
func (listedSignal) required() bool { return true }

func (listedSignal) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int { return 10 }

func (listedSignal) query(_ sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string {
	return listedSignalRule + shard.Selector()
}

func (listedSignal) historyQuery(promclient.Shard, time.Duration) string { return "" }

func (listedSignal) contract() PromContract {
	return PromContract{Rules: []string{listedSignalRule}, Labels: shardLabels}
}

func (listedSignal) collect(samples []promclient.ShardSample) signalValues {
	killedAt := make(listedValues, len(samples))
	for _, s := range samples {
		if s.Metric == listedSignalRule {
			killedAt[s.Container] = s.Value
		}
	}
	return killedAt
}

func (listedSignal) slot() slot { return slot{role: limitAdjuster, resource: memoryResource} }

func (l listedSignal) contribute(sc stageContext) (contribution, bool) {
	killedAt := valuesOf[listedValues](sc.inputs, l)[sc.container.Name]
	if killedAt == 0 {
		return contribution{}, false
	}
	return contribution{value: killedAt * l.factor}, true
}

func (l listedSignal) stage(_ stageContext, value float64, determined bool) sustainv1alpha1.TraceStage {
	return sustainv1alpha1.TraceStage{
		Stage: "listed",
		Value: memoryResource.quantity(value),
		Facts: map[string]string{"factor": strconv.FormatFloat(l.factor, 'g', -1, 64), "determined": strconv.FormatBool(determined)},
	}
}

// withSignals makes list the signals for the duration of the test: listing a
// signal is the one change a new signal makes outside its own module.
func withSignals(t *testing.T, list ...signal) {
	t.Helper()
	listed := signals
	signals = list
	t.Cleanup(func() { signals = listed })
}

// Listing a signal is enough for PromInputs to fetch it, in shards sized by
// its own cost and under its failure policy, and for Compute to run its stage
// in its slot from the values it fetched, next to every other signal's, with
// its own trace entry and no CRD change: as a memory limit adjuster it raises
// the limit the strategy set and determines it, never lowers a limit already
// above, and never adds one to a container without.
func TestSignals_AListedSignalIsFetchedAndComputed(t *testing.T) {
	withSignals(t, cpuUsage, memoryUsage, oomFloor{}, listedSignal{factor: 2})
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		switch {
		case q.signal == "memory" && slices.Equal(q.names, []string{"api"}):
			return []string{
				sample("", "api", "app", 100*mib), sample("", "api", "big", 400*mib), sample("", "api", "quiet", 100*mib),
			}, true
		case q.signal == "listed" && slices.Contains(q.names, "bad"):
			return nil, false
		case q.signal == "listed":
			return []string{sample(listedSignalRule, "api", "app", 128*mib), sample(listedSignalRule, "api", "big", 128*mib)}, true
		}
		return nil, true
	})
	cfg := promInputsCfg()
	cfg.Memory.Limits.EqualsToRequest = true

	got := NewPromInputs(c, 100).FetchInputs(context.Background(), now, cfg, reqsFor(1, "api", "bad", "web"))

	if n := prom.count("cpu", 1); n != 3 {
		t.Errorf("cpu: %d one-identity queries, want 3 at 60 samples each against a budget of 100", n)
	}
	if shards := prom.shardsFor("listed"); len(shards) == 0 || strings.Join(shards[0], ",") != "api,bad,web" {
		t.Errorf("listed shards = %v, want all three in the first: it costs 10 samples each", shards)
	}
	if r := got[prodID("bad")]; r.Err == nil || !strings.Contains(r.Err.Error(), "listed") {
		t.Errorf("bad = %+v, want the listed signal's error: it is required", r)
	}
	if r := got[prodID("web")]; r.Err != nil {
		t.Errorf("web = %+v, want inputs", r)
	}
	api := got[prodID("api")]
	if api.Err != nil {
		t.Fatalf("api = %+v, want inputs", api)
	}

	res := Compute(Request{containers: containers("app", "big", "quiet"), inputs: api.Inputs, resources: cfg, now: now})

	assertQty(t, "app limit", res.Recs["app"].MemoryLimit, "256Mi")
	app := res.Traces["app"].Memory
	assertStages(t, "app limit", app.Limit, "equalsToRequest", "listed")
	assertQty(t, "app listed", app.Limit[1].Value, "256Mi")
	assertFacts(t, app.Limit[1], map[string]string{"factor": "2", "determined": "true"})

	assertQty(t, "big limit", res.Recs["big"].MemoryLimit, "400Mi")
	assertFacts(t, res.Traces["big"].Memory.Limit[1], map[string]string{"factor": "2", "determined": "false"})

	assertStages(t, "quiet limit", res.Traces["quiet"].Memory.Limit, "equalsToRequest")

	unlimited := Compute(Request{containers: containers("app"), inputs: api.Inputs, now: now})
	if rec := unlimited.Recs["app"]; rec.MemoryLimit != nil || rec.RemoveMemoryLimit {
		t.Errorf("app without a limit = %+v, want it to keep having none", rec)
	}
}

// probe is a signal that exists only in this file: in its slot it contributes
// nothing to any container, a value no other stage ever loses to, and keeps
// the context of each of its stages, contribution and trace entry alike.
type probe struct {
	label string
	at    slot
	seen  *[]stageContext
}

// noValues are the values of a signal that reads nothing.
type noValues struct{}

func (noValues) observed() []string { return nil }
func (noValues) excusesAge() bool   { return false }

func (p probe) name() string                                                  { return p.label }
func (probe) required() bool                                                  { return false }
func (probe) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int        { return 0 }
func (probe) query(sustainv1alpha1.ResourcesConfigs, promclient.Shard) string { return "" }
func (probe) historyQuery(promclient.Shard, time.Duration) string             { return "" }
func (probe) contract() PromContract                                          { return PromContract{} }
func (probe) collect([]promclient.ShardSample) signalValues                   { return noValues{} }
func (p probe) slot() slot                                                    { return p.at }

func (p probe) contribute(sc stageContext) (contribution, bool) {
	*p.seen = append(*p.seen, sc)
	return contribution{}, true
}

func (p probe) stage(sc stageContext, value float64, _ bool) sustainv1alpha1.TraceStage {
	*p.seen = append(*p.seen, sc)
	return sustainv1alpha1.TraceStage{Stage: p.label, Value: cpuResource.quantity(value)}
}

// Every stage of a signal sizes with one context, on either side of the chain:
// the resource's Policy config, the container as the identity holds it with
// its requests and limits, and the instant computed as of; on the limit side
// the final request too.
func TestSignals_StagesSizeWithTheirContext(t *testing.T) {
	var requestSide, limitSide []stageContext
	withSignals(t, cpuUsage,
		probe{label: "request", at: slot{role: adjuster, resource: cpuResource}, seen: &requestSide},
		probe{label: "limit", at: slot{role: limitAdjuster, resource: cpuResource}, seen: &limitSide})
	app := limited(corev1.ResourceList{corev1.ResourceCPU: qty("100m")}, corev1.ResourceList{corev1.ResourceCPU: qty("1")})
	cfg := sustainv1alpha1.ResourceConfig{
		Window:   "24h",
		Requests: sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](50)},
		Limits:   sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true},
	}

	res := Compute(Request{
		containers: []corev1.Container{app},
		inputs:     NewInputs(WithCPUUsage(promclient.ContainerValues{"app": 0.2})),
		resources:  sustainv1alpha1.ResourcesConfigs{CPU: cfg},
		now:        now,
	})

	request := res.Recs["app"].CPURequest
	assertQty(t, "request", request, "300m")
	for side, seen := range map[string][]stageContext{"request": requestSide, "limit": limitSide} {
		if len(seen) != 2 {
			t.Fatalf("%s side: %d stages ran, want the contribution and the trace entry", side, len(seen))
		}
		for _, sc := range seen {
			if !reflect.DeepEqual(sc.config, cfg) || !reflect.DeepEqual(sc.container, app) || !sc.now.Equal(now) {
				t.Errorf("%s side: context = %+v, want the CPU config, the container and the instant", side, sc)
			}
			switch {
			case side == "request" && sc.request != nil:
				t.Errorf("request side: request = %s, want none before it is computed", sc.request)
			case side == "limit" && (sc.request == nil || sc.request.Cmp(*request) != 0):
				t.Errorf("limit side: request = %v, want the final %s", sc.request, request)
			}
		}
	}
}

// fixedBase is a CPU base signal that exists only in this file, contributing
// value to every container, with a trace entry saying whether it determined
// the request.
type fixedBase struct {
	label string
	value float64
}

func (f fixedBase) name() string                                                  { return f.label }
func (fixedBase) required() bool                                                  { return false }
func (fixedBase) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int        { return 0 }
func (fixedBase) query(sustainv1alpha1.ResourcesConfigs, promclient.Shard) string { return "" }
func (fixedBase) historyQuery(promclient.Shard, time.Duration) string             { return "" }
func (fixedBase) contract() PromContract                                          { return PromContract{} }

func (fixedBase) collect([]promclient.ShardSample) signalValues { return noValues{} }

func (fixedBase) slot() slot { return slot{role: base, resource: cpuResource} }

func (f fixedBase) contribute(stageContext) (contribution, bool) {
	return contribution{value: f.value, anchors: true}, true
}

func (f fixedBase) stage(_ stageContext, value float64, determined bool) sustainv1alpha1.TraceStage {
	return sustainv1alpha1.TraceStage{
		Stage: f.label,
		Value: cpuResource.quantity(value),
		Facts: map[string]string{"determined": strconv.FormatBool(determined)},
	}
}

// Base signals combine by max: the highest contribution sets the request
// whatever order they are listed in, a tie goes to the first listed, and the
// trace marks the winner, and only it, as the determining stage.
func TestSignals_BaseSignalsCombineByMax(t *testing.T) {
	low, high := fixedBase{"low", 0.1}, fixedBase{"high", 0.3}
	cases := []struct {
		name        string
		listed      []signal
		wantRequest string
		wantWinner  string
	}{
		{"low listed first", []signal{low, high}, "300m", "high"},
		{"high listed first", []signal{high, low}, "300m", "high"},
		{"tie", []signal{fixedBase{"first", 0.2}, fixedBase{"second", 0.2}}, "200m", "first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSignals(t, tc.listed...)

			res := Compute(Request{containers: containers("app"), inputs: NewInputs(), now: now})

			assertQty(t, "request", res.Recs["app"].CPURequest, tc.wantRequest)
			stages := res.Traces["app"].CPU.Request[:len(tc.listed)]
			for _, st := range stages {
				if want := strconv.FormatBool(st.Stage == tc.wantWinner); st.Facts["determined"] != want {
					t.Errorf("%s determined = %s, want %s: %s won", st.Stage, st.Facts["determined"], want, tc.wantWinner)
				}
			}
		})
	}
}

var ruleRef = regexp.MustCompile(`k8s_sustain:[a-z0-9_:]+`)

// rulesIn lists, sorted, every rule query names.
func rulesIn(query string) []string {
	return slices.Compact(slices.Sorted(slices.Values(ruleRef.FindAllString(query, -1))))
}

// Listing a signal is enough for its contract to be checked against the
// chart, and each contract declares exactly the rules its signal's query
// reads, and as history rules exactly those its history read alone reads.
func TestSignals_ContractDeclaresTheRulesItsQueryReads(t *testing.T) {
	withSignals(t, append(slices.Clone(signals), listedSignal{})...)

	contracts := PromContracts()

	if len(contracts) != len(signals) {
		t.Fatalf("%d contracts, want one per signal (%d)", len(contracts), len(signals))
	}
	for i, s := range signals {
		c := contracts[i]
		if c.Signal != s.name() {
			t.Errorf("contract %d is %q's, want %q's", i, c.Signal, s.name())
		}
		read := rulesIn(s.query(sustainv1alpha1.ResourcesConfigs{}, dottedShard))
		if want := slices.Sorted(slices.Values(c.Rules)); !slices.Equal(read, want) {
			t.Errorf("%s reads %v, its contract declares %v", s.name(), read, want)
		}
		history := rulesIn(s.historyQuery(dottedShard, time.Minute))
		if want := slices.Sorted(slices.Values(c.HistoryRules)); !slices.Equal(history, want) {
			t.Errorf("%s's history reads %v, its contract declares %v", s.name(), history, want)
		}
	}
}

// Each signal's collect reads a sample of every rule its contract declares,
// history rules included, named as Prometheus returns it: with the test
// above, a series its queries can return is never dropped for a metric name
// its collect does not handle.
func TestSignals_CollectReadsEveryRuleItsContractDeclares(t *testing.T) {
	for _, s := range signals {
		for _, rule := range slices.Concat(s.contract().Rules, s.contract().HistoryRules) {
			t.Run(s.name()+"/"+rule, func(t *testing.T) {
				none := s.collect(nil)

				read := s.collect([]promclient.ShardSample{{Identity: prodID("api"), Container: "app", Metric: rule, Value: 1}})

				if reflect.DeepEqual(read, none) {
					t.Errorf("collect left %+v as no samples would: it drops %s", read, rule)
				}
			})
		}
	}
}
