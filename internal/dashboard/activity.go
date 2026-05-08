package dashboard

import (
	"net/http"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// activityListLimit caps the per-page Event fetch from the API server.
// k8s-sustain emits events sparsely; a single page is enough to backfill
// the dashboard's activity feed without ever pulling the entire cluster's
// Event history into memory.
const activityListLimit = 500

type activityItem struct {
	Timestamp string `json:"timestamp"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

func (s *Server) handleSummaryActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 100 {
		limit = l
	}

	var list corev1.EventList
	if err := s.K8sClient.List(r.Context(), &list, client.Limit(activityListLimit)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := []activityItem{}
	for _, e := range list.Items {
		if e.Source.Component != "k8s-sustain" {
			continue
		}
		items = append(items, activityItem{
			Timestamp: e.LastTimestamp.Format("2006-01-02T15:04:05Z"),
			Namespace: e.InvolvedObject.Namespace,
			Kind:      e.InvolvedObject.Kind,
			Name:      e.InvolvedObject.Name,
			Reason:    e.Reason,
			Message:   e.Message,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Timestamp > items[j].Timestamp })
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
