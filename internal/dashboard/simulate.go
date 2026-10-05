package dashboard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

type simulationResult struct {
	// Policy is the Policy the Simulation started from, empty when the
	// identity has no governing Policy and none was chosen.
	Policy string `json:"policy,omitempty"`
	// Effective is what it ran with: the Policy's settings with the override
	// merged in and every default explicit.
	Effective          simulationSettings                        `json:"effective"`
	Outcome            sustainv1alpha1.RecommendationOutcome     `json:"outcome"`
	Containers         map[string]simulationContainerResult      `json:"containers"`
	Trace              map[string]sustainv1alpha1.ContainerTrace `json:"trace,omitempty"`
	InitContainers     []string                                  `json:"initContainers,omitempty"`
	CPUSeries          promclient.ContainerTimeSeries            `json:"cpuSeries"`
	MemSeries          promclient.ContainerTimeSeries            `json:"memorySeries"`
	Resources          map[string]containerResources             `json:"resources,omitempty"`
	CPURequests        promclient.ContainerTimeSeries            `json:"cpuRequests,omitempty"`
	MemoryRequests     promclient.ContainerTimeSeries            `json:"memoryRequests,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries            `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries            `json:"memoryRecommendations,omitempty"`
}

type simulationContainerResult struct {
	CPURequest         string  `json:"cpuRequest"`
	MemoryRequest      string  `json:"memoryRequest"`
	CPULimit           string  `json:"cpuLimit,omitempty"`
	MemoryLimit        string  `json:"memoryLimit,omitempty"`
	CPULimitRemoved    bool    `json:"cpuLimitRemoved,omitempty"`
	MemoryLimitRemoved bool    `json:"memoryLimitRemoved,omitempty"`
	CPUUsageCores      float64 `json:"cpuUsageCores,omitempty"`
	MemoryUsageBytes   float64 `json:"memoryUsageBytes,omitempty"`
}

// errBasePolicy is a Policy a Simulation should start from that could not be
// read.
var errBasePolicy = errors.New("reading the Policy to simulate")

// runSimulation computes the Request the controller would make of the
// identity, under the starting Policy's settings with the override merged in:
// with no override and the governing Policy, exactly the controller's, live
// OOM kills aside. An identity the inventory does not hold is simulated from
// whatever Prometheus reports for it.
func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	key := promclient.WorkloadIdentity{Namespace: req.Namespace, OwnerKind: req.OwnerKind, OwnerName: req.OwnerName}
	id, err := s.identity(ctx, key)
	switch {
	case apierrors.IsNotFound(err):
		id = &inventory.Identity{Key: key}
	case err != nil:
		return nil, fmt.Errorf("reading the workload identity: %w", err)
	}
	policy := cmp.Or(req.Policy, id.Policy)
	base, err := s.policySettings(ctx, policy)
	if err != nil {
		return nil, err
	}
	settings, err := base.override(req.Override)
	if err != nil {
		return nil, err
	}

	var tr promclient.TimeRange
	if req.FromTs > 0 && req.ToTs > 0 {
		tr = promclient.TimeRange{Start: time.Unix(req.FromTs, 0), End: time.Unix(req.ToTs, 0)}
	} else {
		tr, err = promclient.TimeRangeFromWindow(recommender.ResourceWindow(req.Window), time.Now())
		if err != nil {
			return nil, fmt.Errorf("resolve chart window: %w", err)
		}
	}

	a := recommender.Assemble(ctx, s.Inputs, autoscaler.NewNamespacedSnapshot(s.K8sClient), settings.rightSizing(), []*inventory.Identity{id})[0]
	if a.Err != nil {
		return nil, a.Err
	}
	res := recommender.Compute(a.Request)
	resources := a.Request.Resources()
	settings.ResourcesConfigs = resources

	step := cmp.Or(req.Step, "5m")
	ns, kind, name := key.Namespace, key.OwnerKind, key.OwnerName
	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(ctx, ns, kind, name, tr, step)
	if err != nil {
		return nil, fmt.Errorf("cpu range query: %w", err)
	}
	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(ctx, ns, kind, name, tr, step)
	if err != nil {
		return nil, fmt.Errorf("memory range query: %w", err)
	}
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(ctx, ns, kind, name, tr, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(ctx, ns, kind, name, tr, step)

	// A kept request is recommended nothing, so it has no series to draw.
	var cpuRecSeries, memRecSeries promclient.ContainerTimeSeries
	if !resources.CPU.Requests.KeepRequest {
		cpuRecSeries, _ = s.PromClient.QueryWorkloadCPURecommendationRangeByContainer(ctx, ns, kind, name,
			recommender.PercentileQuantile(resources.CPU.Requests.Percentile), resources.CPU.Window, tr, step)
	}
	if !resources.Memory.Requests.KeepRequest {
		memRecSeries, _ = s.PromClient.QueryWorkloadMemoryRecommendationRangeByContainer(ctx, ns, kind, name,
			recommender.PercentileQuantile(resources.Memory.Requests.Percentile), resources.Memory.Window, tr, step)
	}

	return &simulationResult{
		Policy:             policy,
		Effective:          settings,
		Outcome:            simulationOutcome(res.Outcome),
		Containers:         simulationContainers(res, a.Request.Inputs()),
		Trace:              res.Traces,
		InitContainers:     initContainerNamesOf(id),
		CPUSeries:          cpuSeries,
		MemSeries:          memSeries,
		Resources:          containerResourcesOf(id),
		CPURequests:        cpuRequests,
		MemoryRequests:     memRequests,
		CPURecommendations: recommendationSeries(cpuRecSeries, a.Request, false),
		MemRecommendations: recommendationSeries(memRecSeries, a.Request, true),
	}, nil
}

// policySettings reads the settings of the Policy named name; none for an
// empty name, which leaves every setting to its default.
func (s *Server) policySettings(ctx context.Context, name string) (simulationSettings, error) {
	if name == "" {
		return simulationSettings{}, nil
	}
	var policy sustainv1alpha1.Policy
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: name}, &policy); err != nil {
		return simulationSettings{}, fmt.Errorf("%w %q: %w", errBasePolicy, name, err)
	}
	return settingsOf(policy.Spec.RightSizing), nil
}

// simulationContainers renders the computed recommendations next to the raw
// usage they were derived from.
func simulationContainers(res recommender.Result, inputs *recommender.WorkloadInputs) map[string]simulationContainerResult {
	out := make(map[string]simulationContainerResult, len(res.Recs))
	for name, rec := range res.Recs {
		cr := simulationContainerResult{
			CPUUsageCores:      inputs.CPUPerPod[name],
			MemoryUsageBytes:   inputs.MemPerPod[name],
			CPULimitRemoved:    rec.RemoveCPULimit,
			MemoryLimitRemoved: rec.RemoveMemoryLimit,
		}
		if rec.CPURequest != nil {
			cr.CPURequest = rec.CPURequest.String()
		}
		if rec.MemoryRequest != nil {
			cr.MemoryRequest = rec.MemoryRequest.String()
		}
		if rec.CPULimit != nil {
			cr.CPULimit = rec.CPULimit.String()
		}
		if rec.MemoryLimit != nil {
			cr.MemoryLimit = rec.MemoryLimit.String()
		}
		out[name] = cr
	}
	return out
}

// recommendationSeries computes, at each raw percentile point, the request the
// identity's Request would have computed had the point been its percentile,
// with the container's OOM kills, so the chart shows exactly what would be
// applied at each step.
func recommendationSeries(series promclient.ContainerTimeSeries, req recommender.Request, memory bool) promclient.ContainerTimeSeries {
	if series == nil {
		return nil
	}
	out := make(promclient.ContainerTimeSeries, len(series))
	for name, points := range series {
		clamped := make([]promclient.TimeValue, len(points))
		for i, p := range points {
			point := recommender.WorkloadInputs{OOM: map[string]recommender.OOM{}}
			if oom, ok := req.Inputs().OOM[name]; ok {
				point.OOM[name] = oom
			}
			if memory {
				point.MemPerPod = promclient.ContainerValues{name: p.Value}
			} else {
				point.CPUPerPod = promclient.ContainerValues{name: p.Value}
			}
			rec := recommender.Compute(req.WithInputs(&point)).Recs[name]
			var v float64
			switch {
			case memory && rec.MemoryRequest != nil:
				v = float64(rec.MemoryRequest.Value())
			case !memory && rec.CPURequest != nil:
				v = float64(rec.CPURequest.MilliValue()) / 1000.0
			}
			clamped[i] = promclient.TimeValue{Timestamp: p.Timestamp, Value: v}
		}
		out[name] = clamped
	}
	return out
}
