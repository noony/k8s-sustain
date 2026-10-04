package recommender

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const listedSignalRule = "k8s_sustain:listed_signal"

// listedSignal is a signal that exists only in this file: a required CPU
// adjuster costing ten samples per container, raising every container's CPU
// request to floor. It records what PromInputs and Compute hand it.
type listedSignal struct {
	floor float64

	mu        sync.Mutex
	collected []promclient.ShardSample
	recorded  []bool
}

func (*listedSignal) name() string   { return "listed" }
func (*listedSignal) required() bool { return true }

func (*listedSignal) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int { return 10 }

func (*listedSignal) query(_ sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string {
	return listedSignalRule + shard.Selector()
}

func (l *listedSignal) collect(_ *WorkloadInputs, samples []promclient.ShardSample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.collected = append(l.collected, samples...)
}

func (*listedSignal) observed(*WorkloadInputs) []string { return nil }

func (*listedSignal) slot() slot { return slot{role: adjuster, resource: cpuResource} }

func (l *listedSignal) contribute(*WorkloadInputs, string) (contribution, bool) {
	return contribution{value: l.floor, anchors: true}, true
}

func (l *listedSignal) record(_ *sustainv1alpha1.ResourceTrace, _ float64, determined bool) {
	l.recorded = append(l.recorded, determined)
}

// listSignal adds s to signals for the duration of the test, the one change a
// new signal makes outside its own module.
func listSignal(t *testing.T, s signal) {
	t.Helper()
	listed := signals
	signals = append(slices.Clone(signals), s)
	t.Cleanup(func() { signals = listed })
}

// Listing a signal is enough for PromInputs to fetch it: shards sized by its
// own cost, its samples handed to it, its failure policy applied.
func TestSignals_PromInputsFetchesAListedSignal(t *testing.T) {
	listed := &listedSignal{}
	listSignal(t, listed)
	prom, c := startFakeShardProm(t, func(q shardQuery) ([]string, bool) {
		if q.signal != "listed" {
			return nil, true
		}
		if slices.Contains(q.names, "bad") {
			return nil, false
		}
		return []string{sample(listedSignalRule, "api", "app", 7)}, true
	})

	got := NewPromInputs(c, 100).FetchInputs(context.Background(), promInputsCfg(), reqsFor(1, "api", "bad", "web"))

	if n := prom.count("cpu", 1); n != 3 {
		t.Errorf("cpu: %d one-identity queries, want 3 at 60 samples each against a budget of 100", n)
	}
	if shards := prom.shardsFor("listed"); len(shards) == 0 || strings.Join(shards[0], ",") != "api,bad,web" {
		t.Errorf("listed shards = %v, want all three in the first: it costs 10 samples each", shards)
	}
	if len(listed.collected) != 1 || listed.collected[0].Identity != prodID("api") || listed.collected[0].Value != 7 {
		t.Errorf("collected = %+v, want api's one sample", listed.collected)
	}
	if r := got[prodID("bad")]; r.Err == nil || !strings.Contains(r.Err.Error(), "listed") {
		t.Errorf("bad = %+v, want the listed signal's error: it is required", r)
	}
	for _, n := range []string{"api", "web"} {
		if r := got[prodID(n)]; r.Err != nil {
			t.Errorf("%s = %+v, want inputs", n, r)
		}
	}
}

// Listing a signal is enough for Compute to run its stage in its slot and
// record its trace entry: as a CPU adjuster it raises the usage percentile,
// determines the request, and alone justifies one for a container without
// usage.
func TestSignals_ComputeRunsAListedSignalsStage(t *testing.T) {
	listed := &listedSignal{floor: 0.5}
	listSignal(t, listed)

	res := Compute(Request{
		Containers: containers("app", "idle"),
		Inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 0.1}},
	})

	for _, name := range []string{"app", "idle"} {
		assertQty(t, name+" cpu", res.Recs[name].CPURequest, "500m")
	}
	assertQty(t, "app percentile", res.Containers["app"].Trace.CPU.Percentile, "100m")
	if !slices.Equal(listed.recorded, []bool{true, true}) {
		t.Errorf("recorded = %v, want the stage to have determined both requests", listed.recorded)
	}
}
