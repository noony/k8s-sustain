package recommendertest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
		Set(known, recommender.WithCPUUsage(promclient.ContainerValues{"app": 1})).
		Fail(broken, boom)

	cfg := sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Window: "24h"}}

	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	got := s.FetchInputs(context.Background(), at, cfg, []recommender.InputsRequest{
		{Identity: known, Containers: 1}, {Identity: broken}, {Identity: unknown},
	})

	if r := got[known]; r.Err != nil || !reflect.DeepEqual(r.Inputs, recommender.NewInputs(recommender.WithCPUUsage(promclient.ContainerValues{"app": 1}))) {
		t.Errorf("known = %+v, want its inputs", r)
	}
	if r := got[broken]; !errors.Is(r.Err, boom) || r.Inputs != nil {
		t.Errorf("broken = %+v, want its error", r)
	}
	if r := got[unknown]; r.Err != nil || !reflect.DeepEqual(r.Inputs, recommender.NewInputs()) {
		t.Errorf("unknown = %+v, want empty inputs", r)
	}
	if calls := s.Calls(); len(calls) != 1 || len(calls[0].Requests) != 3 || calls[0].Resources.CPU.Window != "24h" || !calls[0].At.Equal(at) {
		t.Errorf("calls = %+v, want one call of three requests as of its instant, under its settings", calls)
	}
	if !s.Requested(unknown) {
		t.Error("Requested(unknown) = false after it was fetched")
	}
}

func TestStaticInputs_AnswersEveryHistoryRequestAndRecordsIt(t *testing.T) {
	known := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "known"}
	broken := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "broken"}
	historyless := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "historyless"}
	unknown := promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "unknown"}
	end := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	span := recommender.SpanOf(end.Add(-time.Minute), end, time.Minute)
	history := recommender.NewHistory([]recommender.HistoryPoint{
		{At: end, Inputs: recommender.NewInputs(recommender.WithCPUUsage(promclient.ContainerValues{"app": 1}),
			recommender.WithOOMKills(map[string]recommender.OOM{"app": {Kills: 1, KillsInStep: 1}}))},
	})
	boom, gone := errors.New("boom"), errors.New("gone")
	s := NewStaticInputs().
		SetHistory(known, history).
		Fail(broken, boom).
		Set(historyless, recommender.WithCPUUsage(promclient.ContainerValues{"app": 1})).
		FailHistory(historyless, gone)
	cfg := sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Window: "24h"}}

	got := s.FetchHistory(context.Background(), span, cfg, []recommender.InputsRequest{
		{Identity: known, Containers: 1}, {Identity: broken}, {Identity: historyless}, {Identity: unknown},
	})

	if r := got[known]; r.Err != nil || r.History != history {
		t.Errorf("known = %+v, want its history", r)
	}
	if r := got[broken]; !errors.Is(r.Err, boom) || r.History != nil {
		t.Errorf("broken = %+v, want its error", r)
	}
	if r := got[historyless]; !errors.Is(r.Err, gone) || r.History != nil {
		t.Errorf("historyless = %+v, want its history's error", r)
	}
	if r := s.FetchInputs(context.Background(), end, cfg, []recommender.InputsRequest{{Identity: historyless}})[historyless]; r.Err != nil {
		t.Errorf("historyless inputs = %+v, want its inputs: only its history fails", r)
	}
	empty := recommender.NewHistory([]recommender.HistoryPoint{
		{At: span.Start, Inputs: recommender.NewInputs()}, {At: end, Inputs: recommender.NewInputs()},
	})
	if r := got[unknown]; r.Err != nil || !reflect.DeepEqual(r.History, empty) {
		t.Errorf("unknown = %+v, want empty inputs at every instant", r)
	}
	if calls := s.HistoryCalls(); len(calls) != 1 || len(calls[0].Requests) != 4 || calls[0].Span != span || calls[0].Resources.CPU.Window != "24h" {
		t.Errorf("history calls = %+v, want one call of four requests over its span, under its settings", calls)
	}
}
