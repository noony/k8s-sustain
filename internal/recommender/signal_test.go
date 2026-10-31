package recommender

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const listedSignalRule = "k8s_sustain:listed_signal"

// listedSignal is a signal that exists only in this file: a required memory
// limit adjuster costing ten samples per container, raising a container's
// memory limit to factor times the limit its rule says the container was
// killed at. A signal defined in a test cannot add a field to WorkloadInputs,
// so it keeps its values in the OOM floor's, and a test that fetches it lists
// it instead of the OOM floor.
type listedSignal struct{ factor float64 }

func (listedSignal) name() string   { return "listed" }
func (listedSignal) required() bool { return true }

func (listedSignal) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int { return 10 }

func (listedSignal) query(_ sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string {
	return listedSignalRule + shard.Selector()
}

func (listedSignal) contract() PromContract {
	return PromContract{Rules: []string{listedSignalRule}, Labels: shardLabels}
}

func (listedSignal) collect(in *WorkloadInputs, samples []promclient.ShardSample) {
	killedAt := make(map[string]OOM, len(samples))
	for _, s := range samples {
		if s.Metric == listedSignalRule {
			killedAt[s.Container] = OOM{LimitBytes: s.Value}
		}
	}
	in.OOM = killedAt
}

func (listedSignal) observed(*WorkloadInputs) []string { return nil }
func (listedSignal) excusesAge(*WorkloadInputs) bool   { return false }

func (listedSignal) slot() slot { return slot{role: limitAdjuster, resource: memoryResource} }

func (l listedSignal) contribute(sc stageContext) (contribution, bool) {
	o, ok := sc.inputs.OOM[sc.container.Name]
	if !ok || o.LimitBytes == 0 {
		return contribution{}, false
	}
	return contribution{value: o.LimitBytes * l.factor}, true
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
// in its slot from the inputs fetched, with its own trace entry and no CRD
// change: as a memory limit adjuster it raises the limit the strategy set and
// determines it, never lowers a limit already above, and never adds one to a
// container without.
func TestSignals_AListedSignalIsFetchedAndComputed(t *testing.T) {
	withSignals(t, cpuUsage, memoryUsage, listedSignal{factor: 2})
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

	got := NewPromInputs(c, 100).FetchInputs(context.Background(), cfg, reqsFor(1, "api", "bad", "web"))

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

var ruleRef = regexp.MustCompile(`k8s_sustain:[a-z0-9_:]+`)

// Listing a signal is enough for its contract to be checked against the
// chart, and each contract declares exactly the rules its signal's query
// reads.
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
		read := ruleRef.FindAllString(s.query(sustainv1alpha1.ResourcesConfigs{}, dottedShard), -1)
		if got, want := slices.Sorted(slices.Values(read)), slices.Sorted(slices.Values(c.Rules)); !slices.Equal(got, want) {
			t.Errorf("%s reads %v, its contract declares %v", s.name(), got, want)
		}
	}
}

// Each signal's collect reads a sample of every rule its contract declares,
// named as Prometheus returns it: with the test above, a series its query can
// return is never dropped for a metric name its collect does not handle.
func TestSignals_CollectReadsEveryRuleItsContractDeclares(t *testing.T) {
	for _, s := range signals {
		for _, rule := range s.contract().Rules {
			t.Run(s.name()+"/"+rule, func(t *testing.T) {
				var none, read WorkloadInputs
				s.collect(&none, nil)

				s.collect(&read, []promclient.ShardSample{{Identity: prodID("api"), Container: "app", Metric: rule, Value: 1}})

				if reflect.DeepEqual(read, none) {
					t.Errorf("collect left %+v as no samples would: it drops %s", read, rule)
				}
			})
		}
	}
}
