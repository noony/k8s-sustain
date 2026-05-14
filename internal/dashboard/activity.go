package dashboard

import (
	"net/http"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// activityLimitDefault and activityLimitMax bound the per-request page size.
const (
	activityLimitDefault = 20
	activityLimitMax     = 100
)

// activityListLimit caps the per-page Event fetch from the API server.
// k8s-sustain emits events sparsely; a single page is enough to backfill
// the dashboard's activity feed without ever pulling the entire cluster's
// Event history into memory.
const activityListLimit = 500

func parseActivityLimit(s string) (int, *paramError) {
	if s == "" {
		return activityLimitDefault, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > activityLimitMax {
		return 0, badParam("limit", "invalid limit %q: must be 1..%d", s, activityLimitMax)
	}
	return n, nil
}

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
	limit, perr := parseActivityLimit(r.URL.Query().Get("limit"))
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
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
