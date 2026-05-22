package dashboard

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

type simulationResult struct {
	Containers         map[string]simulationContainerResult `json:"containers"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	CPUSeries          promclient.ContainerTimeSeries       `json:"cpuSeries"`
	MemSeries          promclient.ContainerTimeSeries       `json:"memorySeries"`
	Resources          map[string]containerResources        `json:"resources,omitempty"`
	CPURequests        promclient.ContainerTimeSeries       `json:"cpuRequests,omitempty"`
	MemoryRequests     promclient.ContainerTimeSeries       `json:"memoryRequests,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

type simulationContainerResult struct {
	CPURequest       string  `json:"cpuRequest"`
	MemoryRequest    string  `json:"memoryRequest"`
	CPULimit         string  `json:"cpuLimit,omitempty"`
	MemoryLimit      string  `json:"memoryLimit,omitempty"`
	CPULimitRemoved  bool    `json:"cpuLimitRemoved,omitempty"`
	MemoryLimitRemoved bool  `json:"memoryLimitRemoved,omitempty"`
	CPUUsageCores    float64 `json:"cpuUsageCores,omitempty"`
	MemoryUsageBytes float64 `json:"memoryUsageBytes,omitempty"`
}

func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	cpuCfg := buildRequestsConfig(req.CPU)
	memCfg := buildRequestsConfig(req.Memory)
	cpuLimCfg := buildLimitsConfig(req.CPU.Limits)
	memLimCfg := buildLimitsConfig(req.Memory.Limits)

	cpuQuantile := recommender.PercentileQuantile(cpuCfg.Percentile)
	memQuantile := recommender.PercentileQuantile(memCfg.Percentile)

	// Per-resource windows for recommendation computation
	cpuWindowStr := req.CPU.Window
	if cpuWindowStr == "" {
		cpuWindowStr = req.Window
	}
	memWindowStr := req.Memory.Window
	if memWindowStr == "" {
		memWindowStr = req.Window
	}
	cpuWindow := recommender.ResourceWindow(cpuWindowStr)
	memWindow := recommender.ResourceWindow(memWindowStr)

	// Chart time range (top-level Window controls what's displayed on graphs)
	timeRange := recommender.ResourceWindow(req.Window)

	containers, oomSignal, recentOOM, err := s.buildContainerRecommendations(ctx,
		req.Namespace, req.OwnerKind, req.OwnerName,
		cpuCfg, memCfg, cpuWindow, memWindow,
	)
	if err != nil {
		return nil, err
	}

	// Query time-series for graphs (use chart time range)
	step := req.Step
	if step == "" {
		step = "5m"
	}
	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)
	if err != nil {
		return nil, fmt.Errorf("cpu range query: %w", err)
	}
	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)
	if err != nil {
		return nil, fmt.Errorf("memory range query: %w", err)
	}

	resources := s.getContainerResources(ctx, req.Namespace, req.OwnerKind, req.OwnerName)

	// Layer per-container limit recommendations on top of the request map.
	// The request strings are already final (clamped, MiB-rounded); reparsing
	// them is cheap and keeps the shared builder limit-agnostic.
	for name, result := range containers {
		curReq, curLim := currentQuantities(resources[name])
		if result.CPURequest != "" {
			if cpuQty, err := resource.ParseQuantity(result.CPURequest); err == nil {
				lim := recommender.ComputeLimit(&cpuQty, curReq.cpu, curLim.cpu, cpuLimCfg)
				if lim.Remove {
					result.CPULimitRemoved = true
				} else if lim.Quantity != nil {
					result.CPULimit = lim.Quantity.String()
				}
			}
		}
		if result.MemoryRequest != "" {
			if memQty, err := resource.ParseQuantity(result.MemoryRequest); err == nil {
				lim := recommender.ComputeLimit(&memQty, curReq.mem, curLim.mem, memLimCfg)
				if lim.Remove {
					result.MemoryLimitRemoved = true
				} else if lim.Quantity != nil {
					result.MemoryLimit = lim.Quantity.String()
				}
			}
		}
		containers[name] = result
	}

	// Fetch historical resource request time-series (best-effort, use chart time range)
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)

	// Sliding-window recommendation time-series
	cpuRecSeries, _ := s.PromClient.QueryCPURecommendationRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, cpuQuantile, string(cpuWindow), string(timeRange), step)
	memRecSeries, _ := s.PromClient.QueryMemoryRecommendationRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, memQuantile, string(memWindow), string(timeRange), step)

	cpuRecSeries = applyCPUClampingToSeries(cpuRecSeries, cpuCfg)
	memRecSeries = applyMemoryClampingToSeries(memRecSeries, memCfg, oomSignal, recentOOM)

	initContainers := s.getInitContainerNames(ctx, req.Namespace, req.OwnerKind, req.OwnerName)

	return &simulationResult{
		Containers:         containers,
		InitContainers:     initContainers,
		CPUSeries:          cpuSeries,
		MemSeries:          memSeries,
		Resources:          resources,
		CPURequests:        cpuRequests,
		MemoryRequests:     memRequests,
		CPURecommendations: cpuRecSeries,
		MemRecommendations: memRecSeries,
	}, nil
}

// applyCPUClampingToSeries runs each raw percentile point through the real
// recommender so the chart shows the exact value that would be applied to a
// container's CPU request (ceil-to-millicore, headroom, min/max clamping).
func applyCPUClampingToSeries(series promclient.ContainerTimeSeries, cfg sustainv1alpha1.ResourceRequestsConfig) promclient.ContainerTimeSeries {
	if series == nil {
		return nil
	}
	result := make(promclient.ContainerTimeSeries, len(series))
	for name, points := range series {
		clamped := make([]promclient.TimeValue, len(points))
		for i, p := range points {
			var v float64
			if qty := recommender.ComputeCPURequest(p.Value, cfg); qty != nil {
				v = float64(qty.MilliValue()) / 1000.0
			}
			clamped[i] = promclient.TimeValue{Timestamp: p.Timestamp, Value: v}
		}
		result[name] = clamped
	}
	return result
}

// applyMemoryClampingToSeries is the memory counterpart of
// applyCPUClampingToSeries. The OOM-aware floor (max(peak, oomLimit × BumpFactor))
// is applied per container so the chart line tracks what the controller would
// actually apply.
func applyMemoryClampingToSeries(series promclient.ContainerTimeSeries, cfg sustainv1alpha1.ResourceRequestsConfig, oomSignal promclient.OOMSignal, recentOOM bool) promclient.ContainerTimeSeries {
	if series == nil {
		return nil
	}
	result := make(promclient.ContainerTimeSeries, len(series))
	for name, points := range series {
		clamped := make([]promclient.TimeValue, len(points))
		oom := recommender.NewOOMSignal(recentOOM, oomSignal.PeakMemoryBytes[name], oomSignal.OOMLimitBytes[name])
		for i, p := range points {
			var v float64
			if qty := recommender.ComputeMemoryRequestWithOOM(p.Value, oom, cfg); qty != nil {
				v = float64(qty.Value())
			}
			clamped[i] = promclient.TimeValue{Timestamp: p.Timestamp, Value: v}
		}
		result[name] = clamped
	}
	return result
}

func buildRequestsConfig(cfg simulateResourceConfig) sustainv1alpha1.ResourceRequestsConfig {
	rc := sustainv1alpha1.ResourceRequestsConfig{
		Percentile: cfg.Percentile,
		Headroom:   cfg.Headroom,
	}
	// Quantity parsing tolerated to fail silently here — handleSimulate
	// pre-validates with ParseQuantity, so reaching this code with an
	// unparseable value is impossible. Use ParseQuantity instead of
	// MustParse so a future caller that skips the handler can't trigger a
	// panic and surface as an HTTP 500.
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

type quantPair struct {
	cpu *resource.Quantity
	mem *resource.Quantity
}

// currentQuantities parses the current container request/limit strings into
// Quantity pointers (nil when absent or unparseable). Used to feed
// ComputeLimit's keepLimitRequestRatio branch with the workload's live values.
func currentQuantities(cr containerResources) (req quantPair, lim quantPair) {
	if cr.CPURequest != "" {
		if q, err := resource.ParseQuantity(cr.CPURequest); err == nil {
			req.cpu = &q
		}
	}
	if cr.MemoryRequest != "" {
		if q, err := resource.ParseQuantity(cr.MemoryRequest); err == nil {
			req.mem = &q
		}
	}
	if cr.CPULimit != "" {
		if q, err := resource.ParseQuantity(cr.CPULimit); err == nil {
			lim.cpu = &q
		}
	}
	if cr.MemoryLimit != "" {
		if q, err := resource.ParseQuantity(cr.MemoryLimit); err == nil {
			lim.mem = &q
		}
	}
	return
}
