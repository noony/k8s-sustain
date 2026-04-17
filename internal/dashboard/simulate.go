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
	Containers map[string]simulationContainerResult `json:"containers"`
	CPUSeries  promclient.ContainerTimeSeries       `json:"cpuSeries"`
	MemSeries  promclient.ContainerTimeSeries       `json:"memorySeries"`
	Resources  map[string]containerResources        `json:"resources,omitempty"`
}

type simulationContainerResult struct {
	CPURequest    string `json:"cpuRequest"`
	MemoryRequest string `json:"memoryRequest"`
}

func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	cpuCfg := buildRequestsConfig(req.CPU)
	memCfg := buildRequestsConfig(req.Memory)

	cpuQuantile := recommender.PercentileQuantile(cpuCfg.PercentilePercentage)
	memQuantile := recommender.PercentileQuantile(memCfg.PercentilePercentage)
	window := recommender.ResourceWindow(req.Window)

	// Query single-value percentiles for recommendations
	cpuValues, err := s.PromClient.QueryCPUByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, cpuQuantile, window)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	memValues, err := s.PromClient.QueryMemoryByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, memQuantile, window)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	// Query time-series for graphs
	step := req.Step
	if step == "" {
		step = "5m"
	}
	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, window, step)
	if err != nil {
		return nil, fmt.Errorf("cpu range query: %w", err)
	}
	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, window, step)
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

	return &simulationResult{
		Containers: containers,
		CPUSeries:  cpuSeries,
		MemSeries:  memSeries,
		Resources:  resources,
	}, nil
}

func buildRequestsConfig(cfg simulateResourceConfig) sustainv1alpha1.ResourceRequestsConfig {
	rc := sustainv1alpha1.ResourceRequestsConfig{
		PercentilePercentage: cfg.PercentilePercentage,
		HeadroomPercentage:   cfg.HeadroomPercentage,
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
