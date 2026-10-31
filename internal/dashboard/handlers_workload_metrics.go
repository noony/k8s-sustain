package dashboard

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

func (s *Server) handleWorkloadMetrics(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	q := r.URL.Query()
	tr, perr := parseTimeRange(q, "168h", time.Now())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "5m")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	ctx := r.Context()
	key := promclient.WorkloadIdentity{Namespace: namespace, OwnerKind: kind, OwnerName: name}

	// Only cpu/memory usage failures abort the response; the rest are
	// best-effort.
	var (
		cpuSeries, memSeries promclient.ContainerTimeSeries
		running              runningSeries
		oomEvents            []promclient.OOMKill
		cpuErr, memErr       error
	)
	var wg sync.WaitGroup
	wg.Go(func() {
		cpuSeries, cpuErr = s.PromClient.QueryRangeByLabel(ctx,
			perContainer("avg", promclient.MetricContainerCPUUsageByWorkloadRate1m, key), "container", tr, step)
	})
	wg.Go(func() {
		memSeries, memErr = s.PromClient.QueryRangeByLabel(ctx,
			perContainer("avg", promclient.MetricContainerMemoryByWorkloadBytes, key), "container", tr, step)
	})
	wg.Go(func() { oomEvents = s.oomKills(ctx, key, tr, step) })
	wg.Go(func() { running = s.running(ctx, key, tr, step) })
	wg.Wait()

	if cpuErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cpu range query: %v", cpuErr))
		return
	}
	if memErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("memory range query: %v", memErr))
		return
	}

	// A failed read is tolerated: resources and init containers come back nil.
	id, err := s.identity(ctx, key)
	if err != nil {
		s.Logger.Error(err, "failed to read the workload identity", "namespace", namespace, "kind", kind, "name", name)
	}
	resources := containerResourcesOf(id)
	initContainers := initContainerNamesOf(id)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"cpu":            cpuSeries,
		"memory":         memSeries,
		"resources":      resources,
		"cpuRequests":    running.cpuRequests,
		"memoryRequests": running.memoryRequests,
		"cpuLimits":      running.cpuLimits,
		"memoryLimits":   running.memoryLimits,
		"oomEvents":      oomEvents,
		"initContainers": initContainers,
	})
}

// perContainer renders rule for the identity key, aggregated per container by
// agg.
func perContainer(agg, rule string, key promclient.WorkloadIdentity) string {
	return fmt.Sprintf("%s by (container) (%s%s)", agg, rule, promclient.WorkloadSelector(key.Namespace, key.OwnerKind, key.OwnerName))
}

// runningSeries are the requests and limits an identity's containers ran
// with over a range, per container.
type runningSeries struct {
	cpuRequests, memoryRequests, cpuLimits, memoryLimits promclient.ContainerTimeSeries
}

// running reads the requests and limits key's containers ran with over r,
// the highest of its pods' at each step. A series that cannot be read is
// left nil.
func (s *Server) running(ctx context.Context, key promclient.WorkloadIdentity, r promclient.TimeRange, step string) runningSeries {
	var out runningSeries
	var wg sync.WaitGroup
	for rule, into := range map[string]*promclient.ContainerTimeSeries{
		promclient.MetricContainerCPURequestsByWorkloadCores:    &out.cpuRequests,
		promclient.MetricContainerMemoryRequestsByWorkloadBytes: &out.memoryRequests,
		promclient.MetricContainerCPULimitsByWorkloadCores:      &out.cpuLimits,
		promclient.MetricContainerMemoryLimitsByWorkloadBytes:   &out.memoryLimits,
	} {
		wg.Go(func() {
			*into, _ = s.PromClient.QueryRangeByLabel(ctx, perContainer("max", rule, key), "container", r, step)
		})
	}
	wg.Wait()
	return out
}

// oomKills reads key's OOM kills over r, each dated at the end of the step it
// fell in, none when they cannot be read: kube-state-metrics may be missing,
// and the page renders without them.
func (s *Server) oomKills(ctx context.Context, key promclient.WorkloadIdentity, r promclient.TimeRange, step string) []promclient.OOMKill {
	stepDur, err := promclient.ParseStep(step)
	if err != nil {
		return nil
	}
	selector := promclient.WorkloadSelector(key.Namespace, key.OwnerKind, key.OwnerName)
	perStep, err := s.PromClient.QueryRangeByLabel(ctx, promclient.OOMKillsPerStep(selector, stepDur), "container", r, step)
	if err != nil {
		return nil
	}
	var out []promclient.OOMKill
	for _, container := range slices.Sorted(maps.Keys(perStep)) {
		out = append(out, promclient.OOMEventsOf(container, perStep[container])...)
	}
	return out
}

type containerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// containerResourcesOf returns the requests and limits each of the identity's
// containers runs with, nil when the identity is unknown.
func containerResourcesOf(id *inventory.Identity) map[string]containerResources {
	if id == nil {
		return nil
	}
	all := append(slices.Clone(id.Containers), id.InitContainers...)
	if len(all) == 0 {
		// Keep the nil map so the JSON stays null rather than {}.
		return nil
	}
	result := make(map[string]containerResources, len(all))
	for _, c := range all {
		cpuReq, cpuLim, memReq, memLim := resourceStrings(c)
		result[c.Name] = containerResources{
			CPURequest:    cpuReq,
			CPULimit:      cpuLim,
			MemoryRequest: memReq,
			MemoryLimit:   memLim,
		}
	}
	return result
}

func initContainerNamesOf(id *inventory.Identity) []string {
	if id == nil || len(id.InitContainers) == 0 {
		return nil
	}
	initCs := id.InitContainers
	out := make([]string, len(initCs))
	for i, c := range initCs {
		out[i] = c.Name
	}
	return out
}
