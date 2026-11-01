package recommender

import (
	"context"
	"maps"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// WorkloadInputs is one identity's recommendation inputs: the values of every
// signal, keyed by the signal, each of the signal's own type. Outside the
// recommender it answers only the questions every reader shares (Resolved,
// Usage), and an InputsFetcher other than PromInputs builds it with NewInputs.
// It never changes once built.
type WorkloadInputs struct {
	values map[string]signalValues
}

// signalValues is one signal's values for one identity. Its zero value holds
// nothing, as a signal's values from no samples do.
type signalValues interface {
	// observed lists the containers the values give something to recommend
	// from.
	observed() []string
	// excusesAge reports whether the values let a Too young identity through
	// the age gate.
	excusesAge() bool
}

// valuesOf is the values in holds of s, the zero V when it holds none.
func valuesOf[V signalValues](in *WorkloadInputs, s signal) V {
	v, _ := in.values[s.name()].(V)
	return v
}

func (in *WorkloadInputs) set(s signal, v signalValues) {
	in.values[s.name()] = v
}

// with is a copy of in holding v as s's values. in itself is left untouched.
func (in *WorkloadInputs) with(s signal, v signalValues) *WorkloadInputs {
	out := &WorkloadInputs{values: make(map[string]signalValues, len(signals))}
	if in != nil {
		maps.Copy(out.values, in.values)
	}
	out.set(s, v)
	return out
}

// emptyInputs holds every signal's values from no samples, what PromInputs
// answers for an identity Prometheus has none of.
func emptyInputs() *WorkloadInputs {
	in := &WorkloadInputs{values: make(map[string]signalValues, len(signals))}
	for _, s := range signals {
		in.set(s, s.collect(nil))
	}
	return in
}

// InputsOption sets one signal's values in the inputs NewInputs builds. Each
// signal's module exports the one that sets its values.
type InputsOption func(*WorkloadInputs)

func setting(s signal, v signalValues) InputsOption {
	return func(in *WorkloadInputs) { in.set(s, v) }
}

// NewInputs builds inputs holding the values opts set and every other
// signal's values from no samples, so they read exactly as the inputs
// PromInputs fetches. It is for an InputsFetcher serving canned inputs, such
// as recommendertest.StaticInputs.
func NewInputs(opts ...InputsOption) *WorkloadInputs {
	in := emptyInputs()
	for _, o := range opts {
		o(in)
	}
	return in
}

// InputsRequest asks for one identity's recommendation inputs.
type InputsRequest struct {
	Identity promclient.WorkloadIdentity
	// Containers is the identity's container count, a size hint for batching.
	// Zero means unknown.
	Containers int
}

// InputsResult is one identity's inputs, or the error that made them
// unavailable. Inputs is non-nil exactly when Err is nil.
type InputsResult struct {
	Inputs *WorkloadInputs
	Err    error
}

// InputsFetcher fetches the recommendation inputs of many identities at once,
// all under one ResourcesConfigs. The result holds exactly one entry per
// requested identity. An identity with no samples gets empty Inputs, not an
// error: an error means its inputs could not be read at all.
type InputsFetcher interface {
	FetchInputs(ctx context.Context, cfg sustainv1alpha1.ResourcesConfigs, reqs []InputsRequest) map[promclient.WorkloadIdentity]InputsResult
}
