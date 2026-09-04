package dashboard

import (
	"context"
	"fmt"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"golang.org/x/sync/errgroup"
)

// buildContainerRecommendations runs the dashboard's recommendation pipeline
// for a single workload: per-container CPU/memory percentile queries plus the
// fail-open OOM signal, then the OOM-aware request computation for every
// observed container.
//
// Limits and chart series are deliberately left to callers (see runSimulation).
// The OOM signal is returned so simulate.go can reuse its per-container recency
// when clamping recommendation time-series.
func (s *Server) buildContainerRecommendations(
	ctx context.Context,
	namespace, kind, name string,
	cpuCfg, memCfg sustainv1alpha1.ResourceRequestsConfig,
	cpuWindow, memWindow string,
) (map[string]simulationContainerResult, promclient.OOMSignal, error) {
	cpuQuantile := recommender.PercentileQuantile(cpuCfg.Percentile)
	memQuantile := recommender.PercentileQuantile(memCfg.Percentile)

	// The CPU, memory, and OOM queries are independent Prometheus round-trips,
	// so overlap them to cut dashboard latency from the sum of three trips to
	// the slowest one (mirrors the webhook handler's FetchWorkloadInputs path).
	var (
		cpuValues map[string]float64
		memValues map[string]float64
		oomSignal promclient.OOMSignal
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		v, err := s.PromClient.QueryCPUByContainer(gctx, namespace, kind, name, cpuQuantile, cpuWindow)
		if err != nil {
			return fmt.Errorf("cpu query: %w", err)
		}
		cpuValues = v
		return nil
	})
	g.Go(func() error {
		v, err := s.PromClient.QueryMemoryByContainer(gctx, namespace, kind, name, memQuantile, memWindow)
		if err != nil {
			return fmt.Errorf("memory query: %w", err)
		}
		memValues = v
		return nil
	})
	g.Go(func() error {
		// Fail-open, mirroring the controller's OOM-driven memory floor: the
		// error is swallowed so it never cancels the CPU/memory queries.
		signal, err := s.PromClient.QueryWorkloadOOMSignal(gctx, namespace, kind, name)
		if err == nil {
			oomSignal = signal
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, promclient.OOMSignal{}, err
	}

	// Include OOM peaks in the container set so a crash-looping container
	// with no usage samples still gets a memory recommendation anchored on
	// the kernel-observed peak. OOM recency is per-container: only containers
	// that actually OOMed are added (and floored below) — a sibling's OOM
	// never inflates an innocent container.
	allContainers := make(map[string]struct{})
	for n := range cpuValues {
		allContainers[n] = struct{}{}
	}
	for n := range memValues {
		allContainers[n] = struct{}{}
	}
	for n := range oomSignal.PeakMemoryBytes {
		if oomSignal.OOMCounts[n] > 0 {
			allContainers[n] = struct{}{}
		}
	}

	containers := make(map[string]simulationContainerResult, len(allContainers))
	for n := range allContainers {
		cr := simulationContainerResult{}
		if cores, ok := cpuValues[n]; ok {
			cr.CPUUsageCores = cores
			if qty := recommender.ComputeCPURequest(cores, cpuCfg); qty != nil {
				cr.CPURequest = qty.String()
			}
		}
		memBytes, hasUsage := memValues[n]
		_, hasPeak := oomSignal.PeakMemoryBytes[n]
		recentOOM := oomSignal.OOMCounts[n] > 0
		if hasUsage {
			cr.MemoryUsageBytes = memBytes
		}
		if hasUsage || (recentOOM && hasPeak) {
			oom := recommender.NewOOMSignal(recentOOM, oomSignal.PeakMemoryBytes[n], oomSignal.OOMLimitBytes[n])
			if qty := recommender.ComputeMemoryRequestWithOOM(memBytes, oom, memCfg); qty != nil {
				cr.MemoryRequest = qty.String()
			}
		}
		containers[n] = cr
	}

	return containers, oomSignal, nil
}
