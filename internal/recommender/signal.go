package recommender

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// A signal is an observed input that contributes one stage to a
// Recommendation. It owns everything about that input: the Policy settings it
// reads, the PromQL that reads it for a shard of identities and what that
// costs, how samples become per-container values, whether an identity can be
// recommended without them, and its stage and trace entry. PromInputs fetches
// every signal in signals and Compute runs their stages, so adding a signal
// is adding its module and listing it.
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
	// collect replaces the signal's values in in with those of one identity's
	// samples. No samples leaves it holding empty values.
	collect(in *WorkloadInputs, samples []promclient.ShardSample)
	// observed lists the containers in holds values of the signal for.
	observed(in *WorkloadInputs) []string
	// slot is where the signal's stage runs.
	slot() slot
	// contribute is the signal's value for one container's request before
	// headroom, and false when it has none for that container.
	contribute(in *WorkloadInputs, container string) (contribution, bool)
	// record writes the signal's trace entry for the value it contributed.
	// determined says that value reached the final request: no other signal
	// beat it and no operator bound replaced it.
	record(t *sustainv1alpha1.ResourceTrace, value float64, determined bool)
}

// signals is every signal, in the order their stages run within a role.
var signals = []signal{cpuUsage, memoryUsage, oomFloor{}}

// role orders the stages of a resource: every base signal before every
// adjuster.
type role int

const (
	// base signals set a request from observed usage.
	base role = iota
	// adjusters raise the value the base signals set, never lower it.
	adjuster
)

// slot places a signal's stage: its role, and the resource whose request it
// shapes.
type slot struct {
	role     role
	resource resourceKind
}

// contribution is a signal's value for one container's request.
type contribution struct {
	value float64
	// anchors says the value justifies a request even with no usage behind
	// it.
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
