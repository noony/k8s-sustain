// Package recommendertest provides an in-memory recommender.InputsFetcher for
// tests of the packages that fetch recommendation inputs.
package recommendertest

import (
	"context"
	"slices"
	"sync"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

// StaticInputs serves canned inputs per identity. An identity given inputs
// with Set gets them, one given an error with Fail gets the error, and any
// other identity gets empty inputs, as Prometheus answers for an identity with
// no samples. Every request is recorded so a test can assert on what was
// fetched.
type StaticInputs struct {
	mu     sync.Mutex
	inputs map[promclient.WorkloadIdentity]*recommender.WorkloadInputs
	errs   map[promclient.WorkloadIdentity]error
	calls  [][]recommender.InputsRequest
}

var _ recommender.InputsFetcher = (*StaticInputs)(nil)

// NewStaticInputs returns a StaticInputs that knows no identity yet.
func NewStaticInputs() *StaticInputs {
	return &StaticInputs{
		inputs: make(map[promclient.WorkloadIdentity]*recommender.WorkloadInputs),
		errs:   make(map[promclient.WorkloadIdentity]error),
	}
}

// Set serves in for id.
func (s *StaticInputs) Set(id promclient.WorkloadIdentity, in *recommender.WorkloadInputs) *StaticInputs {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs[id] = in
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
	_ sustainv1alpha1.ResourcesConfigs,
	reqs []recommender.InputsRequest,
) map[promclient.WorkloadIdentity]recommender.InputsResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(reqs))
	out := make(map[promclient.WorkloadIdentity]recommender.InputsResult, len(reqs))
	for _, r := range reqs {
		if err, ok := s.errs[r.Identity]; ok {
			out[r.Identity] = recommender.InputsResult{Err: err}
			continue
		}
		in := recommender.WorkloadInputs{}
		if set := s.inputs[r.Identity]; set != nil {
			in = *set
		}
		if in.CPUPerPod == nil {
			in.CPUPerPod = promclient.ContainerValues{}
		}
		if in.MemPerPod == nil {
			in.MemPerPod = promclient.ContainerValues{}
		}
		out[r.Identity] = recommender.InputsResult{Inputs: &in}
	}
	return out
}

// Calls returns the requests of every FetchInputs call, oldest first.
func (s *StaticInputs) Calls() [][]recommender.InputsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Requested reports whether any FetchInputs call asked for id.
func (s *StaticInputs) Requested(id promclient.WorkloadIdentity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.calls {
		for _, r := range call {
			if r.Identity == id {
				return true
			}
		}
	}
	return false
}
