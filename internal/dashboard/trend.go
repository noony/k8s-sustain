package dashboard

import (
	"net/http"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type trendResponse struct {
	CPU    []promclient.TimeValue `json:"cpu"`
	Memory []promclient.TimeValue `json:"memory"`
}

func (s *Server) handleSummaryTrend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "30d"
	}
	step := r.URL.Query().Get("step")
	if step == "" {
		step = "1h"
	}
	w.Header().Set("Cache-Control", "public, max-age=60")

	cpu, _ := s.PromClient.QueryRange(r.Context(), "k8s_sustain:cluster_cpu_savings_cores", window, step)
	mem, _ := s.PromClient.QueryRange(r.Context(), "k8s_sustain:cluster_memory_savings_bytes", window, step)
	if cpu == nil {
		cpu = []promclient.TimeValue{}
	}
	if mem == nil {
		mem = []promclient.TimeValue{}
	}
	writeJSON(w, http.StatusOK, trendResponse{CPU: cpu, Memory: mem})
}
