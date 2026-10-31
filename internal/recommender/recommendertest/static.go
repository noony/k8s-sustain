// Package recommendertest provides an in-memory recommender.InputsFetcher for
// tests of the packages that fetch recommendation inputs.
package recommendertest

import (
	"context"
	"slices"
	"sync"
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

// StaticInputs serves canned inputs per identity, whatever the settings it is
// asked to fetch under. An identity given signal values with Set gets inputs
// holding them, one given an error with Fail gets the error, and any other
// identity gets empty inputs, as Prometheus answers for an identity with no
// samples. Inputs are built with recommender.NewInputs, so they read exactly
// as PromInputs' do. Every call is recorded so a test can assert on what was
// fetched, as of which instant and under which settings.
type StaticInputs struct {
	mu     sync.Mutex
	inputs map[promclient.WorkloadIdentity]*recommender.WorkloadInputs
	errs   map[promclient.WorkloadIdentity]error
	calls  []Call
}

// Call is one FetchInputs call: the instant it fetched as of, the settings it
// fetched under and the requests it made.
type Call struct {
	At        time.Time
	Resources sustainv1alpha1.ResourcesConfigs
	Requests  []recommender.InputsRequest
}

var _ recommender.InputsFetcher = (*StaticInputs)(nil)

// NewStaticInputs returns a StaticInputs that knows no identity yet.
func NewStaticInputs() *StaticInputs {
	return &StaticInputs{
		inputs: make(map[promclient.WorkloadIdentity]*recommender.WorkloadInputs),
		errs:   make(map[promclient.WorkloadIdentity]error),
	}
}

// Set serves id inputs holding values, each set by a signal's option
// (recommender.WithCPUUsage, …), and every other signal's empty values.
func (s *StaticInputs) Set(id promclient.WorkloadIdentity, values ...recommender.InputsOption) *StaticInputs {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs[id] = recommender.NewInputs(values...)
	delete(s.errs, id)
	return s
}

// Fail makes every fetch of id return err.
func (s *StaticInputs) Fail(id promclient.WorkloadIdentity, err error) *StaticInputs {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[id] = err
	delete(s.inputs, id)
	return s
}

// FetchInputs answers every request from what Set and Fail configured.
func (s *StaticInputs) FetchInputs(
	_ context.Context,
	at time.Time,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []recommender.InputsRequest,
) map[promclient.WorkloadIdentity]recommender.InputsResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{At: at, Resources: *cfg.DeepCopy(), Requests: slices.Clone(reqs)})
	out := make(map[promclient.WorkloadIdentity]recommender.InputsResult, len(reqs))
	for _, r := range reqs {
		if err, ok := s.errs[r.Identity]; ok {
			out[r.Identity] = recommender.InputsResult{Err: err}
			continue
		}
		in := s.inputs[r.Identity]
		if in == nil {
			in = recommender.NewInputs()
		}
		out[r.Identity] = recommender.InputsResult{Inputs: in}
	}
	return out
}

// Calls returns every FetchInputs call, oldest first.
func (s *StaticInputs) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Requested reports whether any FetchInputs call asked for id.
func (s *StaticInputs) Requested(id promclient.WorkloadIdentity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.calls {
		for _, r := range call.Requests {
			if r.Identity == id {
				return true
			}
		}
	}
	return false
}
