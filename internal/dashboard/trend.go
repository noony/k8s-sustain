package dashboard

import (
	"context"
	"fmt"
	"net/http"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type trendSeries struct {
	Usage           []promclient.TimeValue `json:"usage"`
	Request         []promclient.TimeValue `json:"request"`
	OriginalRequest []promclient.TimeValue `json:"originalRequest"`
}

type trendResponse struct {
	CPU    trendSeries `json:"cpu"`
	Memory trendSeries `json:"memory"`
}

func (s *Server) handleSummaryTrend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	window, perr := parseDurationParam(q, "window", "30d")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "1h")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")

	// "Original request" only exists for policy-managed workloads. Usage and
	// current-request recording rules are cluster-wide (any pod with an owner
	// mapping), so summing them as-is over-counts and makes Current request
	// appear above Original request even when right-sizing is working. Filter
	// usage and current-request to the same managed-workload scope by joining
	// against the template-* metrics.
	scoped := func(metric, joinKey string) string {
		return fmt.Sprintf(`sum(
			%s
			and on(namespace, owner_kind, owner_name, container)
			%s
		)`, metric, joinKey)
	}
	cpuUsageScoped := scoped(promclient.MetricWorkloadCPUUsageCores, promclient.MetricWorkloadTemplateCPUCores)
	cpuRequestScoped := scoped(promclient.MetricContainerCPURequestsByWorkloadCores, promclient.MetricWorkloadTemplateCPUCores)
	memUsageScoped := scoped(promclient.MetricWorkloadMemoryUsageBytes, promclient.MetricWorkloadTemplateMemoryBytes)
	memRequestScoped := scoped(promclient.MetricContainerMemoryRequestsByWorkloadBytes, promclient.MetricWorkloadTemplateMemoryBytes)

	resp := trendResponse{
		CPU: trendSeries{
			Usage:           s.queryRangeOrEmpty(r.Context(), cpuUsageScoped, window, step),
			Request:         s.queryRangeOrEmpty(r.Context(), cpuRequestScoped, window, step),
			OriginalRequest: s.queryRangeOrEmpty(r.Context(), fmt.Sprintf("sum(%s)", promclient.MetricWorkloadTemplateCPUCores), window, step),
		},
		Memory: trendSeries{
			Usage:           s.queryRangeOrEmpty(r.Context(), memUsageScoped, window, step),
			Request:         s.queryRangeOrEmpty(r.Context(), memRequestScoped, window, step),
			OriginalRequest: s.queryRangeOrEmpty(r.Context(), fmt.Sprintf("sum(%s)", promclient.MetricWorkloadTemplateMemoryBytes), window, step),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) queryRangeOrEmpty(ctx context.Context, expr, window, step string) []promclient.TimeValue {
	v, _ := s.PromClient.QueryRange(ctx, expr, window, step)
	if v == nil {
		return []promclient.TimeValue{}
	}
	return v
}
