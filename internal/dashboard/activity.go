package dashboard

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// activityLimitDefault and activityLimitMax bound the per-request page size.
const (
	activityLimitDefault = 20
	activityLimitMax     = 100
)

// activityListLimit caps the Event fetch from the API server. The list is
// filtered server-side to k8s-sustain's own events via a field selector, so
// this bound only guards against an unexpectedly large backlog of those —
// it never pulls the entire cluster's Event history into memory.
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
	limit, perr := parseActivityLimit(r.URL.Query().Get("limit"))
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	var list corev1.EventList
	opts := []client.ListOption{
		client.Limit(activityListLimit),
		client.MatchingFields{"source": "k8s-sustain"},
	}
	if err := s.K8sClient.List(r.Context(), &list, opts...); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := []activityItem{}
	for _, e := range list.Items {
		items = append(items, activityItem{
			Timestamp: eventTimestamp(e).UTC().Format("2006-01-02T15:04:05Z"),
			Namespace: e.InvolvedObject.Namespace,
			Kind:      e.InvolvedObject.Kind,
			Name:      e.InvolvedObject.Name,
			Reason:    e.Reason,
			Message:   e.Message,
		})
	}
	slices.SortFunc(items, func(a, b activityItem) int { return cmp.Compare(b.Timestamp, a.Timestamp) })
	if len(items) > limit {
		items = items[:limit]
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// eventTimestamp returns the most recent time an Event was observed. Events
// created through the events.k8s.io API (the default for current Kubernetes)
// leave the legacy FirstTimestamp/LastTimestamp fields empty and instead carry
// EventTime, plus Series.LastObservedTime for deduplicated repeats. We prefer
// the freshest populated field so the activity feed sorts by real recency.
func eventTimestamp(e corev1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}
