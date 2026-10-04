package dashboard

import (
	"fmt"
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

	// Only cpu/memory usage failures abort the response; the rest are
	// best-effort.
	var (
		cpuSeries, memSeries                           promclient.ContainerTimeSeries
		cpuRequests, memRequests, cpuLimits, memLimits promclient.ContainerTimeSeries
		oomEvents                                      []promclient.OOMEvent
		cpuErr, memErr                                 error
	)
	var wg sync.WaitGroup
	wg.Go(func() {
		cpuSeries, cpuErr = s.PromClient.QueryCPURangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memSeries, memErr = s.PromClient.QueryMemoryRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		oomEvents, _ = s.PromClient.QueryOOMKillEvents(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		cpuRequests, _ = s.PromClient.QueryCPURequestRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memRequests, _ = s.PromClient.QueryMemoryRequestRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		cpuLimits, _ = s.PromClient.QueryCPULimitRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
	wg.Go(func() {
		memLimits, _ = s.PromClient.QueryMemoryLimitRangeByContainer(ctx, namespace, kind, name, tr, step)
	})
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
	id, err := s.identity(ctx, promclient.WorkloadIdentity{Namespace: namespace, OwnerKind: kind, OwnerName: name})
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
		"cpuRequests":    cpuRequests,
		"memoryRequests": memRequests,
		"cpuLimits":      cpuLimits,
		"memoryLimits":   memLimits,
		"oomEvents":      oomEvents,
		"initContainers": initContainers,
	})
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
