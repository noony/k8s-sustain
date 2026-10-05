package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

type simulationResult struct {
	Containers         map[string]simulationContainerResult `json:"containers"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	TooYoung           bool                                 `json:"tooYoung,omitempty"`
	CPUSeries          promclient.ContainerTimeSeries       `json:"cpuSeries"`
	MemSeries          promclient.ContainerTimeSeries       `json:"memorySeries"`
	Resources          map[string]containerResources        `json:"resources,omitempty"`
	CPURequests        promclient.ContainerTimeSeries       `json:"cpuRequests,omitempty"`
	MemoryRequests     promclient.ContainerTimeSeries       `json:"memoryRequests,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
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

// rightSizing maps a user-supplied request onto the Policy shape. A
// resource's window falls back to the chart window; the assembly supplies the
// final default.
func (req simulateRequest) rightSizing(coordination sustainv1alpha1.AutoscalerCoordination) sustainv1alpha1.RightSizingSpec {
	return sustainv1alpha1.RightSizingSpec{
		ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
			CPU: sustainv1alpha1.ResourceConfig{
				Window:   cmp.Or(req.CPU.Window, req.Window),
				Requests: buildRequestsConfig(req.CPU),
				Limits:   buildLimitsConfig(req.CPU.Limits),
			},
			Memory: sustainv1alpha1.ResourceConfig{
				Window:   cmp.Or(req.Memory.Window, req.Window),
				Requests: buildRequestsConfig(req.Memory),
				Limits:   buildLimitsConfig(req.Memory.Limits),
			},
		},
		AutoscalerCoordination: coordination,
	}
}

// runSimulation resolves the identity and the coordination baseline, then
// simulates the request. Coordination defaults to the governing Policy's
// setting so an untouched simulation matches what the controller does; the
// request can override it.
func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	key := promclient.WorkloadIdentity{Namespace: req.Namespace, OwnerKind: req.OwnerKind, OwnerName: req.OwnerName}
	// A failed read is tolerated: an unknown identity simulates with no
	// resources or init containers rather than failing.
	id, err := s.identity(ctx, key)
	if err != nil {
		s.Logger.Error(err, "failed to read the workload identity", "namespace", req.Namespace, "kind", req.OwnerKind, "name", req.OwnerName)
		id = &inventory.Identity{Key: key}
	}
	var coordination sustainv1alpha1.AutoscalerCoordination
	switch {
	case req.AutoscalerCoordination != nil:
		coordination = *req.AutoscalerCoordination
	default:
		if policy := s.governingPolicy(ctx, id); policy != nil {
			coordination = policy.Spec.RightSizing.AutoscalerCoordination
		}
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

	a := recommender.Assemble(ctx, s.Inputs, autoscaler.NewNamespacedSnapshot(s.K8sClient), req.rightSizing(coordination), []*inventory.Identity{id})[0]
	if a.Err != nil {
		return nil, a.Err
	}
	res := recommender.Compute(a.Request)
	inputs := a.Request.Inputs

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

	resources := a.Request.Resources
	cpuRecSeries, _ := s.PromClient.QueryWorkloadCPURecommendationRangeByContainer(ctx, ns, kind, name,
		recommender.PercentileQuantile(resources.CPU.Requests.Percentile), resources.CPU.Window, tr, step)
	memRecSeries, _ := s.PromClient.QueryWorkloadMemoryRecommendationRangeByContainer(ctx, ns, kind, name,
		recommender.PercentileQuantile(resources.Memory.Requests.Percentile), resources.Memory.Window, tr, step)

	return &simulationResult{
		Containers:         simulationContainers(res, inputs),
		InitContainers:     initContainerNamesOf(id),
		TooYoung:           res.Outcome == recommender.TooYoung,
		CPUSeries:          cpuSeries,
		MemSeries:          memSeries,
		Resources:          containerResourcesOf(id),
		CPURequests:        cpuRequests,
		MemoryRequests:     memRequests,
		CPURecommendations: recommendationSeries(cpuRecSeries, a.Request, false),
		MemRecommendations: recommendationSeries(memRecSeries, a.Request, true),
	}, nil
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
			if oom, ok := req.Inputs.OOM[name]; ok {
				point.OOM[name] = oom
			}
			if memory {
				point.MemPerPod = promclient.ContainerValues{name: p.Value}
			} else {
				point.CPUPerPod = promclient.ContainerValues{name: p.Value}
			}
			at := req
			at.Inputs = &point
			rec := recommender.Compute(at).Recs[name]
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

func buildRequestsConfig(cfg simulateResourceConfig) sustainv1alpha1.ResourceRequestsConfig {
	rc := sustainv1alpha1.ResourceRequestsConfig{
		Percentile: cfg.Percentile,
		Headroom:   cfg.Headroom,
	}
	// handleSimulate pre-validates these, so a parse failure is unreachable;
	// ParseQuantity rather than MustParse keeps a caller that skips the handler
	// from panicking into an HTTP 500.
	if cfg.MinAllowed != nil {
		if q, err := resource.ParseQuantity(*cfg.MinAllowed); err == nil {
			rc.MinAllowed = &q
		}
	}
	if cfg.MaxAllowed != nil {
		if q, err := resource.ParseQuantity(*cfg.MaxAllowed); err == nil {
			rc.MaxAllowed = &q
		}
	}
	return rc
}

func buildLimitsConfig(cfg *simulateLimitsConfig) sustainv1alpha1.ResourceLimitsConfig {
	if cfg == nil {
		return sustainv1alpha1.ResourceLimitsConfig{}
	}
	return sustainv1alpha1.ResourceLimitsConfig{
		EqualsToRequest:       cfg.EqualsToRequest,
		KeepLimit:             cfg.KeepLimit,
		KeepLimitRequestRatio: cfg.KeepLimitRequestRatio,
		NoLimit:               cfg.NoLimit,
		RequestsLimitsRatio:   cfg.RequestsLimitsRatio,
	}
}
