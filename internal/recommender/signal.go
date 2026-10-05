package recommender

import (
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// A signal is an observed input that contributes one stage to a
// Recommendation. It owns everything about that input: the Policy settings it
// reads, the PromQL that reads it for a shard of identities and what that
// costs, how samples become per-container values, whether an identity can be
// recommended without them, whether they excuse an identity's age, its stage
// and trace entry, and what it needs from Prometheus. PromInputs fetches every
// signal in signals and Compute runs their stages, so adding a signal is
// adding its module and listing it.
type signal interface {
	// name labels the signal in errors and logs.
	name() string
	// required is the failure policy: an identity whose values cannot be read
	// gets no inputs at all, rather than being computed without them.
	required() bool
	// samplesPerContainer is what one container costs a shard query under
	// cfg, counted against PromInputs' sample budget.
	samplesPerContainer(cfg sustainv1alpha1.ResourcesConfigs) int
	// query is the PromQL reading the signal for every identity in shard.
	query(cfg sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string
	// contract is what query reads from Prometheus and what that rests on.
	contract() PromContract
	// collect replaces the signal's values in in with those of one identity's
	// samples. No samples leaves it holding empty values.
	collect(in *WorkloadInputs, samples []promclient.ShardSample)
	// observed lists the containers in holds values of the signal for.
	observed(in *WorkloadInputs) []string
	// excusesAge reports whether the signal's values let a Too young identity
	// through the age gate.
	excusesAge(in *WorkloadInputs) bool
	// slot is where the signal's stage runs.
	slot() slot
	// contribute is the signal's value for one container's request before
	// headroom, or for its limit, and false when it has none for that
	// container.
	contribute(in *WorkloadInputs, container string) (contribution, bool)
	// stage is the signal's trace entry for the value it contributed to
	// container. determined says that value reached the final request or
	// limit: no other signal beat it and no operator bound or guard replaced
	// it.
	stage(in *WorkloadInputs, container string, value float64, determined bool) sustainv1alpha1.TraceStage
}

// signals is every signal, in the order their stages run within a role.
var signals = []signal{cpuUsage, memoryUsage, oomFloor{}}

// PromContract is what one signal needs from Prometheus. The chart must
// provide it: hack/signalcontract fails when a rule is missing, aggregates by
// other labels, evaluates at another interval or has no promtool fixture, and
// when a raw metric it rests on is dropped by the bundled scrape keep-lists.
type PromContract struct {
	Signal string
	// Rules are the recording rules the signal's query reads.
	Rules []string
	// Labels are what each of Rules aggregates by, exactly.
	Labels []string
	// Interval is the evaluation interval of Rules the signal's sample cost
	// assumes; zero when the cost does not depend on it.
	Interval time.Duration
	// CAdvisor and KubeStateMetrics are the raw metrics Rules compute from,
	// through every rule they read in turn.
	CAdvisor         []string
	KubeStateMetrics []string
}

// PromContracts lists every signal's PromContract, in signals' order. The
// lists are copies: the signals' queries read the same ones.
func PromContracts() []PromContract {
	out := make([]PromContract, 0, len(signals))
	for _, s := range signals {
		c := s.contract()
		out = append(out, PromContract{
			Signal:           s.name(),
			Rules:            slices.Clone(c.Rules),
			Labels:           slices.Clone(c.Labels),
			Interval:         c.Interval,
			CAdvisor:         slices.Clone(c.CAdvisor),
			KubeStateMetrics: slices.Clone(c.KubeStateMetrics),
		})
	}
	return out
}

// shardLabels are the labels a shard query's samples are attributed by. A
// series missing one is dropped, and a rule keeping one more would hand a
// container several series.
var shardLabels = []string{"namespace", "owner_kind", "owner_name", "container"}

// podWorkloadMetrics are the kube-state-metrics series k8s_sustain:pod_workload
// maps a pod to its identity from. Every rule a signal reads joins it.
var podWorkloadMetrics = []string{"kube_pod_owner", "kube_job_owner", "kube_replicaset_owner", "kube_pod_labels"}

// role orders the stages of a resource: every base signal before every
// adjuster on the request side, then every limit adjuster on the limit side.
type role int

const (
	// base signals set a request from observed usage.
	base role = iota
	// adjusters raise the value the base signals set, never lower it.
	adjuster
	// limitAdjusters raise the limit the Policy's limit strategy set, never
	// lower it, remove it or add one to a container without.
	limitAdjuster
)

// slot places a signal's stage: its role, and the resource it shapes.
type slot struct {
	role     role
	resource resourceKind
}

// inSlot lists the signals whose stage runs in sl, in their listed order.
func inSlot(sl slot) []signal {
	var out []signal
	for _, s := range signals {
		if s.slot() == sl {
			out = append(out, s)
		}
	}
	return out
}

// contribution is a signal's value for one container's request or limit.
type contribution struct {
	value float64
	// anchors says the value justifies a request even with no usage behind
	// it. A limit adjuster's never does.
	anchors bool
}

// observedContainers lists, sorted, every container any signal holds values
// for: all a departed or unknown identity has left to recommend for.
func observedContainers(in *WorkloadInputs) []corev1.Container {
	names := make(map[string]struct{})
	for _, s := range signals {
		for _, n := range s.observed(in) {
			names[n] = struct{}{}
		}
	}
	out := make([]corev1.Container, 0, len(names))
	for _, n := range slices.Sorted(maps.Keys(names)) {
		out = append(out, corev1.Container{Name: n})
	}
	return out
}
