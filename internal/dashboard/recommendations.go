package dashboard

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

// buildContainerRecommendations runs the dashboard's recommendation pipeline
// for a single workload: per-container CPU/memory percentile queries plus the
// fail-open OOM signal, then the shared per-container computation
// (recommender.BuildContainerRecs) for every observed container, so the
// dashboard cannot drift from what the controller would apply.
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
	var inputs recommender.WorkloadInputs
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		v, err := s.PromClient.QueryCPUByContainer(gctx, namespace, kind, name, cpuQuantile, cpuWindow)
		if err != nil {
			return fmt.Errorf("cpu query: %w", err)
		}
		inputs.CPUPerPod = v
		return nil
	})
	g.Go(func() error {
		v, err := s.PromClient.QueryMemoryByContainer(gctx, namespace, kind, name, memQuantile, memWindow)
		if err != nil {
			return fmt.Errorf("memory query: %w", err)
		}
		inputs.MemPerPod = v
		return nil
	})
	g.Go(func() error {
		// Fail-open, mirroring the controller's OOM-driven memory floor: the
		// error is swallowed so it never cancels the CPU/memory queries.
		signal, err := s.PromClient.QueryWorkloadOOMSignal(gctx, namespace, kind, name)
		if err == nil {
			inputs.OOM = signal
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, promclient.OOMSignal{}, err
	}

	// The container set is the union of every series, including OOM anchors
	// for containers that OOMed, so a crash-looping container with no usage
	// samples still gets a memory recommendation. BuildContainerRecs decides
	// per container whether there is enough signal to emit anything.
	names := make(map[string]struct{})
	for n := range inputs.CPUPerPod {
		names[n] = struct{}{}
	}
	for n := range inputs.MemPerPod {
		names[n] = struct{}{}
	}
	for n, count := range inputs.OOM.OOMCounts {
		if count > 0 {
			names[n] = struct{}{}
		}
	}
	containers := make([]corev1.Container, 0, len(names))
	for _, n := range slices.Sorted(maps.Keys(names)) {
		containers = append(containers, corev1.Container{Name: n})
	}

	rsCfg := sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{Requests: cpuCfg},
		Memory: sustainv1alpha1.ResourceConfig{Requests: memCfg},
	}
	recs := recommender.BuildContainerRecs(containers, &inputs, autoscaler.Info{Kind: autoscaler.KindNone}, rsCfg,
		sustainv1alpha1.AutoscalerCoordination{}, recommender.BuildContainerRecsOptions{})

	out := make(map[string]simulationContainerResult, len(names))
	for n := range names {
		cr := simulationContainerResult{
			CPUUsageCores:    inputs.CPUPerPod[n],
			MemoryUsageBytes: inputs.MemPerPod[n],
		}
		if rec, ok := recs[n]; ok {
			if rec.CPURequest != nil {
				cr.CPURequest = rec.CPURequest.String()
			}
			if rec.MemoryRequest != nil {
				cr.MemoryRequest = rec.MemoryRequest.String()
			}
		}
		out[n] = cr
	}
	return out, inputs.OOM, nil
}
