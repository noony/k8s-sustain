package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// ---- Workload recommendations ----

type recommendationResult struct {
	Automated          bool                                 `json:"automated"`
	PolicyName         string                               `json:"policyName,omitempty"`
	Containers         map[string]simulationContainerResult `json:"containers,omitempty"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

func (s *Server) handleWorkloadRecommendations(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	ctx := r.Context()

	w.Header().Set("Cache-Control", "public, max-age=60")

	policyName, err := s.getWorkloadPolicyAnnotation(ctx, namespace, kind, name)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("workload not found: %v", err))
		return
	}

	if policyName == "" {
		writeJSON(w, http.StatusOK, recommendationResult{Automated: false})
		return
	}

	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("policy %q not found: %v", policyName, err))
		return
	}

	req, perr := buildSimulateRequestFromPolicy(r.URL.Query(), policy, namespace, kind, name)
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	result, err := s.runSimulation(ctx, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("computing recommendations: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, recommendationResult{
		Automated:          true,
		PolicyName:         policyName,
		Containers:         result.Containers,
		InitContainers:     result.InitContainers,
		CPURecommendations: result.CPURecommendations,
		MemRecommendations: result.MemRecommendations,
	})
}

// buildSimulateRequestFromPolicy fills out a simulateRequest using a policy's
// resource configs as defaults, then overrides the chart window/step from the
// query string. Returns a paramError on invalid query input.
func buildSimulateRequestFromPolicy(q url.Values, policy *sustainv1alpha1.Policy, namespace, kind, name string) (simulateRequest, *paramError) {
	cpuCfg := policy.Spec.RightSizing.ResourcesConfigs.CPU
	memCfg := policy.Spec.RightSizing.ResourcesConfigs.Memory

	cpuWindow := cpuCfg.Window
	if cpuWindow == "" {
		cpuWindow = "168h"
	}
	memWindow := memCfg.Window
	if memWindow == "" {
		memWindow = "168h"
	}

	chartWindow, perr := parseDurationParam(q, "window", cpuWindow)
	if perr != nil {
		return simulateRequest{}, perr
	}
	step, perr := parseStepParam(q, "5m")
	if perr != nil {
		return simulateRequest{}, perr
	}

	req := simulateRequest{
		Namespace: namespace,
		OwnerKind: kind,
		OwnerName: name,
		Window:    chartWindow,
		Step:      step,
		CPU: simulateResourceConfig{
			Percentile: cpuCfg.Requests.Percentile,
			Headroom:   cpuCfg.Requests.Headroom,
			Window:     cpuWindow,
		},
		Memory: simulateResourceConfig{
			Percentile: memCfg.Requests.Percentile,
			Headroom:   memCfg.Requests.Headroom,
			Window:     memWindow,
		},
	}
	if cpuCfg.Requests.MinAllowed != nil {
		v := cpuCfg.Requests.MinAllowed.String()
		req.CPU.MinAllowed = &v
	}
	if cpuCfg.Requests.MaxAllowed != nil {
		v := cpuCfg.Requests.MaxAllowed.String()
		req.CPU.MaxAllowed = &v
	}
	if memCfg.Requests.MinAllowed != nil {
		v := memCfg.Requests.MinAllowed.String()
		req.Memory.MinAllowed = &v
	}
	if memCfg.Requests.MaxAllowed != nil {
		v := memCfg.Requests.MaxAllowed.String()
		req.Memory.MaxAllowed = &v
	}
	return req, nil
}

// ---- Workload metrics ----

func (s *Server) handleWorkloadMetrics(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	w.Header().Set("Cache-Control", "public, max-age=60")

	q := r.URL.Query()
	window, perr := parseDurationParam(q, "window", "168h")
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
	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(ctx, namespace, kind, name, window, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cpu range query: %v", err))
		return
	}
	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(ctx, namespace, kind, name, window, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("memory range query: %v", err))
		return
	}

	// Best-effort: tolerate Prometheus blips for these supplementary series.
	oomEvents, _ := s.PromClient.QueryOOMKillEvents(ctx, namespace, kind, name, window, step)
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(ctx, namespace, kind, name, window, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(ctx, namespace, kind, name, window, step)

	resources := s.getContainerResources(ctx, namespace, kind, name)
	initContainers := s.getInitContainerNames(ctx, namespace, kind, name)

	writeJSON(w, http.StatusOK, map[string]any{
		"cpu":            cpuSeries,
		"memory":         memSeries,
		"resources":      resources,
		"cpuRequests":    cpuRequests,
		"memoryRequests": memRequests,
		"oomEvents":      oomEvents,
		"initContainers": initContainers,
	})
}

// ---- Container resources / helpers used by metrics + simulate handlers ----

type containerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

func (s *Server) getWorkloadPolicyAnnotation(ctx context.Context, namespace, kind, name string) (string, error) {
	e, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		return "", err
	}
	return e.PolicyAnnotation(), nil
}

func (s *Server) getContainerResources(ctx context.Context, namespace, kind, name string) map[string]containerResources {
	e, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		s.Logger.Error(err, "failed to get workload containers", "namespace", namespace, "kind", kind, "name", name)
		return nil
	}
	all := append([]corev1.Container{}, e.Containers()...)
	all = append(all, e.InitContainers()...)
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

func (s *Server) getInitContainerNames(ctx context.Context, namespace, kind, name string) []string {
	e, err := s.getWorkloadEntry(ctx, namespace, kind, name)
	if err != nil {
		return nil
	}
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
