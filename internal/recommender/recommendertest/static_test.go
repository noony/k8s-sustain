package recommendertest

import (
	"context"
	"errors"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

func TestStaticInputs_AnswersEveryRequestAndRecordsIt(t *testing.T) {
	known := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "known"}
	broken := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "broken"}
	unknown := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "unknown"}
	boom := errors.New("boom")
	s := NewStaticInputs().
		Set(known, &recommender.WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 1}}).
		Fail(broken, boom)

	got := s.FetchInputs(context.Background(), sustainv1alpha1.ResourcesConfigs{}, []recommender.InputsRequest{
		{Identity: known, Containers: 1}, {Identity: broken}, {Identity: unknown},
	})

	if r := got[known]; r.Err != nil || r.Inputs.CPUPerPod["app"] != 1 || r.Inputs.MemPerPod == nil {
		t.Errorf("known = %+v, want its inputs with non-nil maps", r)
	}
	if r := got[broken]; !errors.Is(r.Err, boom) || r.Inputs != nil {
		t.Errorf("broken = %+v, want its error", r)
	}
	if r := got[unknown]; r.Err != nil || r.Inputs == nil || len(r.Inputs.CPUPerPod) != 0 {
		t.Errorf("unknown = %+v, want empty inputs", r)
	}
	if calls := s.Calls(); len(calls) != 1 || len(calls[0]) != 3 {
		t.Errorf("calls = %v, want one call of three requests", calls)
	}
	if !s.Requested(unknown) {
		t.Error("Requested(unknown) = false after it was fetched")
	}
}
