package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

type recommendationResult struct {
	Automated          bool                                 `json:"automated"`
	PolicyName         string                               `json:"policyName,omitempty"`
	Containers         map[string]simulationContainerResult `json:"containers,omitempty"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	TooYoung           bool                                 `json:"tooYoung,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

func (s *Server) handleWorkloadRecommendations(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	ctx := r.Context()

	// The entry is threaded into runSimulationWithEntry so the simulation does
	// not re-Get the object.
	entry, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("workload %s/%s/%s: %v", namespace, kind, name, err))
		return
	}

	// resolveManagingPolicy searches every member behind a grouped identity so
	// this endpoint and /api/workloads reach the same verdict. A Policy List
	// failure fails the request; Automated: false would blame the workload.
	policies, err := s.policiesByName(ctx)
	if err != nil {
		writeK8sGetError(w, err, fmt.Sprintf("listing policies: %v", err))
		return
	}
	policyName, ok := resolveManagingPolicy(entry, policies, s.ExcludedNamespaces)
	if !ok {
		w.Header().Set("Cache-Control", "public, max-age=60")
		writeJSON(w, http.StatusOK, recommendationResult{Automated: false})
		return
	}
	policy := policies[policyName]

	spec, perr := chartParams(r.URL.Query(), policySpec(policy, namespace, kind, name))
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	result, err := s.runSimulationWithEntry(ctx, spec, entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("computing recommendations: %v", err))
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, recommendationResult{
		Automated:          true,
		PolicyName:         policyName,
		Containers:         result.Containers,
		InitContainers:     result.InitContainers,
		TooYoung:           result.TooYoung,
		CPURecommendations: result.CPURecommendations,
		MemRecommendations: result.MemRecommendations,
	})
}

// chartParams overlays the chart window, step and absolute range from the
// query string onto spec. The chart window defaults to the CPU
// recommendation window.
func chartParams(q url.Values, spec simulationSpec) (simulationSpec, *paramError) {
	var perr *paramError
	if spec.window, perr = parseDurationParam(q, recommender.ResourceWindow(spec.resources.CPU.Window)); perr != nil {
		return spec, perr
	}
	if spec.step, perr = parseStepParam(q, "5m"); perr != nil {
		return spec, perr
	}
	if v := q.Get("from"); v != "" {
		spec.fromTs, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("to"); v != "" {
		spec.toTs, _ = strconv.ParseInt(v, 10, 64)
	}
	return spec, nil
}

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

	// A failed Get is tolerated: resources and init containers come back nil.
	entry, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		s.Logger.Error(err, "failed to get workload entry", "namespace", namespace, "kind", kind, "name", name)
		entry = workloadEntry{}
	}
	resources := containerResourcesFromEntry(entry)
	initContainers := initContainerNamesFromEntry(entry)

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

// getWorkloadPolicyAnnotation resolves the Policy that manages a workload, or
// "" when no real object behind its identity both opts into a Policy and
// matches its selector.
func (s *Server) getWorkloadPolicyAnnotation(ctx context.Context, namespace, kind, name string) (string, error) {
	e, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		return "", err
	}
	policies, err := s.policiesByName(ctx)
	if err != nil {
		return "", err
	}
	policyName, ok := resolveManagingPolicy(e, policies, s.ExcludedNamespaces)
	if !ok {
		return "", nil
	}
	return policyName, nil
}

func containerResourcesFromEntry(e workloadEntry) map[string]containerResources {
	all := append([]corev1.Container{}, e.Containers()...)
	all = append(all, e.InitContainers()...)
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

func initContainerNamesFromEntry(e workloadEntry) []string {
	initCs := e.InitContainers()
	if len(initCs) == 0 {
		return nil
	}
	out := make([]string, len(initCs))
	for i, c := range initCs {
		out[i] = c.Name
	}
	return out
}
