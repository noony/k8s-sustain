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
	Containers           map[string]simulationContainerResult `json:"containers"`
	CPUSeries            promclient.ContainerTimeSeries       `json:"cpuSeries"`
	MemSeries            promclient.ContainerTimeSeries       `json:"memorySeries"`
	Resources            map[string]containerResources        `json:"resources,omitempty"`
	CPURequests          promclient.ContainerTimeSeries       `json:"cpuRequests,omitempty"`
	MemoryRequests       promclient.ContainerTimeSeries       `json:"memoryRequests,omitempty"`
	CPURecommendations   promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations   promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

type simulationContainerResult struct {
	CPURequest       string  `json:"cpuRequest"`
	MemoryRequest    string  `json:"memoryRequest"`
	CPUUsageCores    float64 `json:"cpuUsageCores,omitempty"`
	MemoryUsageBytes float64 `json:"memoryUsageBytes,omitempty"`
}

func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	cpuCfg := buildRequestsConfig(req.CPU)
	memCfg := buildRequestsConfig(req.Memory)

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

	// Query single-value percentiles for recommendations (use per-resource windows)
	cpuValues, err := s.PromClient.QueryCPUByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	memValues, err := s.PromClient.QueryMemoryByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
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

	// Compute recommendations per container
	containers := make(map[string]simulationContainerResult)

	// Collect all container names from both CPU and memory
	allContainers := make(map[string]struct{})
	for name := range cpuValues {
		allContainers[name] = struct{}{}
	}
	for name := range memValues {
		allContainers[name] = struct{}{}
	}

	for name := range allContainers {
		result := simulationContainerResult{}
		if cores, ok := cpuValues[name]; ok {
			qty := recommender.ComputeCPURequest(cores, cpuCfg)
			if qty != nil {
				result.CPURequest = qty.String()
			}
		}
		if bytes, ok := memValues[name]; ok {
			qty := recommender.ComputeMemoryRequest(bytes, memCfg)
			if qty != nil {
				result.MemoryRequest = qty.String()
			}
		}
		containers[name] = result
	}

	resources := s.getContainerResources(ctx, req.Namespace, req.OwnerKind, req.OwnerName)

	// Fetch historical resource request time-series (best-effort, use chart time range)
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, timeRange, step)

	// Sliding-window recommendation time-series
	cpuRecSeries, _ := s.PromClient.QueryCPURecommendationRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, cpuQuantile, string(cpuWindow), string(timeRange), step)
	memRecSeries, _ := s.PromClient.QueryMemoryRecommendationRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, memQuantile, string(memWindow), string(timeRange), step)

	// Apply headroom / min / max clamping to recommendation series
	cpuRecSeries = applyClampingToSeries(cpuRecSeries, cpuCfg, true)
	memRecSeries = applyClampingToSeries(memRecSeries, memCfg, false)

	return &simulationResult{
		Containers:         containers,
		CPUSeries:          cpuSeries,
		MemSeries:          memSeries,
		Resources:          resources,
		CPURequests:        cpuRequests,
		MemoryRequests:     memRequests,
		CPURecommendations: cpuRecSeries,
		MemRecommendations: memRecSeries,
	}, nil
}

// applyClampingToSeries applies headroom and min/max clamping to each data point.
// For CPU (isCPU=true), values are in cores; for memory, values are in bytes.
func applyClampingToSeries(series promclient.ContainerTimeSeries, cfg sustainv1alpha1.ResourceRequestsConfig, isCPU bool) promclient.ContainerTimeSeries {
	if series == nil {
		return nil
	}

	var headroom float64
	if cfg.Headroom != nil && *cfg.Headroom > 0 {
		headroom = float64(*cfg.Headroom) / 100.0
	}

	var minVal, maxVal float64
	hasMin, hasMax := false, false
	if cfg.MinAllowed != nil {
		hasMin = true
		if isCPU {
			minVal = float64(cfg.MinAllowed.MilliValue()) / 1000.0
		} else {
			minVal = float64(cfg.MinAllowed.Value())
		}
	}
	if cfg.MaxAllowed != nil {
		hasMax = true
		if isCPU {
			maxVal = float64(cfg.MaxAllowed.MilliValue()) / 1000.0
		} else {
			maxVal = float64(cfg.MaxAllowed.Value())
		}
	}

	result := make(promclient.ContainerTimeSeries, len(series))
	for name, points := range series {
		clamped := make([]promclient.TimeValue, len(points))
		for i, p := range points {
			v := p.Value
			v *= 1.0 + headroom
			if hasMin && v < minVal {
				v = minVal
			}
			if hasMax && v > maxVal {
				v = maxVal
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
	if cfg.MinAllowed != nil {
		q := resource.MustParse(*cfg.MinAllowed)
		rc.MinAllowed = &q
	}
	if cfg.MaxAllowed != nil {
		q := resource.MustParse(*cfg.MaxAllowed)
		rc.MaxAllowed = &q
	}
	return rc
}
