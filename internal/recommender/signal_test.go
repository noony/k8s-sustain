package recommender

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const listedSignalRule = "k8s_sustain:listed_signal"

// listedSignal is a signal that exists only in this file: a required memory
// limit adjuster costing ten samples per container, raising a container's
// memory limit to factor times the limit it was last killed at, read from
// that identity's inputs. It keeps the samples PromInputs hands it.
type listedSignal struct {
	factor float64

	mu        sync.Mutex
	collected []promclient.ShardSample
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
func (*listedSignal) excusesAge(*WorkloadInputs) bool   { return false }

func (*listedSignal) slot() slot { return slot{role: limitAdjuster, resource: memoryResource} }

func (l *listedSignal) contribute(in *WorkloadInputs, container string) (contribution, bool) {
	o, ok := in.OOM[container]
	if !ok || o.LimitBytes == 0 {
		return contribution{}, false
	}
	return contribution{value: o.LimitBytes * l.factor}, true
}

func (l *listedSignal) stage(_ *WorkloadInputs, _ string, value float64, determined bool) sustainv1alpha1.TraceStage {
	return sustainv1alpha1.TraceStage{
		Stage: "listed",
		Value: memoryResource.quantity(value),
		Facts: map[string]string{"factor": strconv.FormatFloat(l.factor, 'g', -1, 64), "determined": strconv.FormatBool(determined)},
	}
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
// record its own trace entry, with no CRD change: as a memory limit adjuster
// it raises the limit the strategy set from each container's own inputs and
// determines it, never raises a limit already above, and never adds one to a
// container without.
func TestSignals_ComputeRunsAListedLimitSignalsStage(t *testing.T) {
	listSignal(t, &listedSignal{factor: 2})
	equal := sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{
		Limits: sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true},
	}}
	in := &WorkloadInputs{
		MemPerPod: promclient.ContainerValues{"app": 100 * mib, "big": 400 * mib, "quiet": 100 * mib},
		OOM:       map[string]OOM{"app": {LimitBytes: 128 * mib}, "big": {LimitBytes: 128 * mib}},
	}

	res := Compute(Request{Containers: containers("app", "big", "quiet"), Inputs: in, Resources: equal, Now: now})

	assertQty(t, "app limit", res.Recs["app"].MemoryLimit, "256Mi")
	app := res.Traces["app"].Memory
	assertStages(t, "app limit", app.Limit, "equalsToRequest", "listed")
	assertQty(t, "app listed", app.Limit[1].Value, "256Mi")
	assertFacts(t, app.Limit[1], map[string]string{"factor": "2", "determined": "true"})

	assertQty(t, "big limit", res.Recs["big"].MemoryLimit, "400Mi")
	assertFacts(t, res.Traces["big"].Memory.Limit[1], map[string]string{"factor": "2", "determined": "false"})

	assertStages(t, "quiet limit", res.Traces["quiet"].Memory.Limit, "equalsToRequest")

	unlimited := Compute(Request{
		Containers: []corev1.Container{{Name: "app"}},
		Inputs:     in,
		Now:        now,
	})
	if rec := unlimited.Recs["app"]; rec.MemoryLimit != nil || rec.RemoveMemoryLimit {
		t.Errorf("app without a limit = %+v, want it to keep having none", rec)
	}
}
