package recommender

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type fakeQuerier struct {
	cpu, mem promclient.ContainerValues
	oom      promclient.OOMSignal
	err      error
	calls    atomic.Int32
}

func (f *fakeQuerier) QueryWorkloadCPUByContainer(context.Context, string, string, string, float64, string) (promclient.ContainerValues, error) {
	f.calls.Add(1)
	return f.cpu, f.err
}

func (f *fakeQuerier) QueryWorkloadMemoryByContainer(context.Context, string, string, string, float64, string) (promclient.ContainerValues, error) {
	f.calls.Add(1)
	return f.mem, f.err
}

func (f *fakeQuerier) QueryWorkloadOOMSignal(context.Context, string, string, string) (promclient.OOMSignal, error) {
	f.calls.Add(1)
	return f.oom, nil
}

func TestCompute_FetchesAndRecommendsEveryObservedContainer(t *testing.T) {
	const mib = 1 << 20
	q := &fakeQuerier{
		cpu: promclient.ContainerValues{"app": 0.5},
		mem: promclient.ContainerValues{"app": 100 * mib},
		oom: promclient.OOMSignal{
			OOMCounts:       promclient.ContainerValues{"crashy": 1},
			PeakMemoryBytes: promclient.ContainerValues{"crashy": 300 * mib},
		},
	}
	res, err := Compute(context.Background(), q, Request{
		Identity:        promclient.WorkloadIdentity{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "web"},
		WorkloadCreated: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if q.calls.Load() != 3 {
		t.Errorf("querier calls = %d, want 3 (cpu, memory, oom)", q.calls.Load())
	}
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "500m" {
		t.Errorf("app cpu = %v, want 500m", got)
	}
	if got := res.Recs["crashy"].MemoryRequest; got == nil || got.String() != "300Mi" {
		t.Errorf("crashy memory = %v, want 300Mi from the OOM peak with no usage samples", got)
	}
	if res.Inputs == nil || res.Inputs.CPUPerPod["app"] != 0.5 {
		t.Errorf("Inputs should carry the fetched usage, got %+v", res.Inputs)
	}
	if res.TooYoung {
		t.Error("an hour-old workload is not too young")
	}
}

func TestCompute_PrefetchedInputsSkipTheQuerier(t *testing.T) {
	q := &fakeQuerier{err: errors.New("must not be called")}
	res, err := Compute(context.Background(), q, Request{
		Containers: []corev1.Container{{Name: "app"}},
		Inputs:     &WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": 1}},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if q.calls.Load() != 0 {
		t.Errorf("querier calls = %d, want 0 with prefetched inputs", q.calls.Load())
	}
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "1" {
		t.Errorf("app cpu = %v, want 1", got)
	}
}

func TestCompute_ExplicitContainersBoundTheResult(t *testing.T) {
	q := &fakeQuerier{cpu: promclient.ContainerValues{"app": 1, "renamed-away": 1}}
	res, err := Compute(context.Background(), q, Request{Containers: []corev1.Container{{Name: "app"}}})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if _, ok := res.Recs["renamed-away"]; ok {
		t.Error("a container Prometheus reports but the workload no longer declares must not be recommended")
	}
	if _, ok := res.Recs["app"]; !ok {
		t.Error("declared container missing from the result")
	}
}

func TestCompute_ReportsTooYoungButStillComputes(t *testing.T) {
	q := &fakeQuerier{cpu: promclient.ContainerValues{"app": 1}}
	res, err := Compute(context.Background(), q, Request{WorkloadCreated: time.Now()})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !res.TooYoung {
		t.Error("a just-created workload should be reported too young")
	}
	if len(res.Recs) != 1 {
		t.Errorf("recs = %v, want the recommendation regardless of age", res.Recs)
	}
}

func TestCompute_UsageQueryFailureIsFatal(t *testing.T) {
	q := &fakeQuerier{err: errors.New("prometheus down")}
	if _, err := Compute(context.Background(), q, Request{}); err == nil {
		t.Fatal("expected the usage query error to surface")
	}
}

func TestCompute_AppliesCoordination(t *testing.T) {
	q := &fakeQuerier{cpu: promclient.ContainerValues{"app": 1}}
	res, err := Compute(context.Background(), q, Request{
		Coordination: sustainv1alpha1.AutoscalerCoordination{Enabled: true},
		AutoInfo:     autoscaler.Info{Kind: autoscaler.KindHPA, ConfiguredTargets: map[string]int32{autoscaler.ResourceCPU: 50}},
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	// 1 core * 110 / 50 = 2.2 cores.
	if got := res.Recs["app"].CPURequest; got == nil || got.String() != "2200m" {
		t.Errorf("coordinated cpu = %v, want 2200m", got)
	}
}
