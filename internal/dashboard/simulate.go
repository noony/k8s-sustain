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

	// Query single-value percentiles for recommendations (use per-resource windows)
	cpuValues, err := s.PromClient.QueryCPUByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	memValues, err := s.PromClient.QueryMemoryByContainer(ctx, req.Namespace, req.OwnerKind, req.OwnerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	// OOM signal — best-effort, fail-open. Mirrors the controller's behavior so
	// the dashboard shows the same memory floor (peak observed bytes) that the
	// controller would actually apply when a workload OOM'd within the 24h
	// lookback. Without this, the displayed recommendation stays at the
	// percentile-based value and never reflects the OOM-driven floor.
	oomSignal, err := s.PromClient.QueryWorkloadOOMSignal(ctx, req.Namespace, req.OwnerKind, req.OwnerName)
	if err != nil {
		oomSignal = promclient.OOMSignal{}
	}
	recentOOM := oomSignal.OOMCount > 0

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

	// Compute recommendations per container
	containers := make(map[string]simulationContainerResult)

	// Collect all container names from CPU, memory, and the OOM peak set.
	// Including OOM peaks ensures a crash-looping container with no usage
	// samples yet still gets a memory recommendation anchored on the peak.
	allContainers := make(map[string]struct{})
	for name := range cpuValues {
		allContainers[name] = struct{}{}
	}
	for name := range memValues {
		allContainers[name] = struct{}{}
	}
	if recentOOM {
		for name := range oomSignal.PeakMemoryBytes {
			allContainers[name] = struct{}{}
		}
	}

	for name := range allContainers {
		result := simulationContainerResult{}
		var cpuQty, memQty *resource.Quantity
		if cores, ok := cpuValues[name]; ok {
			cpuQty = recommender.ComputeCPURequest(cores, cpuCfg)
			if cpuQty != nil {
				result.CPURequest = cpuQty.String()
			}
		}
		// Memory: emit a recommendation when EITHER usage samples are present,
		// OR a recent OOM gives us a peak to anchor on. Mirrors the controller's
		// shape in recommendation_build.go so a crash-looping container that
		// hasn't accumulated usage samples still gets the OOM-driven floor.
		memBytes, hasUsage := memValues[name]
		_, hasPeak := oomSignal.PeakMemoryBytes[name]
		if hasUsage || (recentOOM && hasPeak) {
			oom := recommender.NewOOMSignal(recentOOM, oomSignal.PeakMemoryBytes[name], oomSignal.OOMLimitBytes[name])
			memQty = recommender.ComputeMemoryRequestWithOOM(memBytes, oom, memCfg)
			if memQty != nil {
				result.MemoryRequest = memQty.String()
			}
		}
		curReq, curLim := currentQuantities(resources[name])
		if cpuQty != nil {
			lim := recommender.ComputeLimit(cpuQty, curReq.cpu, curLim.cpu, cpuLimCfg)
			if lim.Remove {
				result.CPULimitRemoved = true
			} else if lim.Quantity != nil {
				result.CPULimit = lim.Quantity.String()
			}
		}
		if memQty != nil {
			lim := recommender.ComputeLimit(memQty, curReq.mem, curLim.mem, memLimCfg)
			if lim.Remove {
				result.MemoryLimitRemoved = true
			} else if lim.Quantity != nil {
				result.MemoryLimit = lim.Quantity.String()
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
