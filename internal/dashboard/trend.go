package dashboard

import (
	"fmt"
	"net/http"
	"time"

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
	tr, perr := parseTimeRange(q, "30d", time.Now())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "1h")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

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

	// Track failed queries so a full Prometheus outage surfaces as a 503
	// instead of a 200 with empty series (indistinguishable from "no data").
	// Partial failures still return the series that succeeded, mirroring
	// handleSummary's graceful degradation.
	const trendQueryCount = 6
	queryErrs := 0
	queryRangeOrEmpty := func(expr string) []promclient.TimeValue {
		v, err := s.PromClient.QueryRange(r.Context(), expr, tr, step)
		if err != nil {
			s.Logger.Error(err, "summary trend query failed", "expr", expr)
			queryErrs++
		}
		if v == nil {
			return []promclient.TimeValue{}
		}
		return v
	}

	resp := trendResponse{
		CPU: trendSeries{
			Usage:           queryRangeOrEmpty(cpuUsageScoped),
			Request:         queryRangeOrEmpty(cpuRequestScoped),
			OriginalRequest: queryRangeOrEmpty(fmt.Sprintf("sum(%s)", promclient.MetricWorkloadTemplateCPUCores)),
		},
		Memory: trendSeries{
			Usage:           queryRangeOrEmpty(memUsageScoped),
			Request:         queryRangeOrEmpty(memRequestScoped),
			OriginalRequest: queryRangeOrEmpty(fmt.Sprintf("sum(%s)", promclient.MetricWorkloadTemplateMemoryBytes)),
		},
	}
	if queryErrs == trendQueryCount {
		writeError(w, http.StatusServiceUnavailable, "prometheus unavailable: all trend queries failed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, resp)
}
