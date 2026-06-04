package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type workloadDetailResponse struct {
	UpdateMode          string                 `json:"updateMode,omitempty"`
	LastRecycledAt      string                 `json:"lastRecycledAt,omitempty"`
	DriftPercent        float64                `json:"driftPercent"`
	OOM24h              int                    `json:"oom24h"`
	Blocked             *workloadDetailBlocked `json:"blocked,omitempty"`
	RecentEvents        []activityItem         `json:"recentEvents"`
	CoordinationFactors *coordinationFactors   `json:"coordinationFactors,omitempty"`
}

type workloadDetailBlocked struct {
	Reason      string `json:"reason"`
	Attempts    int    `json:"attempts"`
	NextRetryAt string `json:"nextRetryAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	w.Header().Set("Cache-Control", "public, max-age=30")

	resp := workloadDetailResponse{}
	resp.UpdateMode = s.lookupUpdateMode(ctx, namespace, kind, name)
	s.fillDetailPrometheusSignals(ctx, &resp, namespace, kind, name)
	resp.RecentEvents = s.recentSustainEvents(ctx, namespace, kind, name, 10)
	if cf := s.fetchCoordinationFactors(ctx, namespace, kind, name); cf != nil {
		resp.CoordinationFactors = cf
	}

	writeJSON(w, http.StatusOK, resp)
}

// lookupUpdateMode resolves the per-kind update mode the workload's owning
// policy specifies, or "" if the workload is unmanaged or the policy can't be
// read.
func (s *Server) lookupUpdateMode(ctx context.Context, namespace, kind, name string) string {
	policyName, _ := s.getWorkloadPolicyAnnotation(ctx, namespace, kind, name)
	if policyName == "" {
		return ""
	}
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: policyName}, policy); err != nil {
		return ""
	}
	mode := updateModeForKind(policy, kind)
	if mode == nil {
		return ""
	}
	return string(*mode)
}

func (s *Server) fillDetailPrometheusSignals(ctx context.Context, resp *workloadDetailResponse, namespace, kind, name string) {
	oomExpr := fmt.Sprintf(`%s{namespace=%q,owner_kind=%q,owner_name=%q}`, promclient.MetricWorkloadOOM24h, namespace, kind, name)
	if v, _ := s.PromClient.QueryInstant(ctx, oomExpr); v > 0 {
		resp.OOM24h = int(v)
	}
	driftExpr := fmt.Sprintf(`max(abs(1 - %s{namespace=%q,owner_kind=%q,owner_name=%q}))`, promclient.MetricWorkloadDriftRatio, namespace, kind, name)
	if v, _ := s.PromClient.QueryInstant(ctx, driftExpr); v > 0 {
		resp.DriftPercent = v * 100
	}
	blockedExpr := fmt.Sprintf(`%s{namespace=%q,owner_kind=%q,owner_name=%q} == 1`, promclient.MetricWorkloadRetryState, namespace, kind, name)
	blockedByReason, _ := s.PromClient.QueryByLabel(ctx, blockedExpr, "reason")
	if len(blockedByReason) == 0 {
		return
	}
	var reason string
	for k := range blockedByReason {
		reason = k
		break
	}
	attemptsExpr := fmt.Sprintf(`%s{namespace=%q,owner_kind=%q,owner_name=%q}`, promclient.MetricWorkloadRetryAttempts, namespace, kind, name)
	attempts, _ := s.PromClient.QueryInstant(ctx, attemptsExpr)
	resp.Blocked = &workloadDetailBlocked{Reason: reason, Attempts: int(attempts)}
}

// recentSustainEvents returns up to `limit` k8s-sustain-emitted events for one
// workload, most-recent first.
func (s *Server) recentSustainEvents(ctx context.Context, namespace, kind, name string, limit int) []activityItem {
	var list corev1.EventList
	// Push the workload identity into the field selector so the API server
	// returns only THIS workload's events. The Event field selectors
	// involvedObject.name/involvedObject.kind are built in, and InNamespace
	// scopes involvedObject.namespace. With the result already narrowed to one
	// workload, the activityListLimit cap can no longer truncate the target's
	// events the way a namespace-wide >500 backlog could. The source filter is
	// kept so only k8s-sustain's own events surface (mirrors handleSummaryActivity).
	_ = s.K8sClient.List(ctx, &list,
		client.InNamespace(namespace),
		client.Limit(activityListLimit),
		client.MatchingFields{
			"source":              "k8s-sustain",
			"involvedObject.kind": kind,
			"involvedObject.name": name,
		},
	)
	// Defensive sort: the API server does not guarantee Events come back ordered
	// by recency, so sort newest-first before applying the keep-limit cap (mirrors
	// the sort in handleSummaryActivity).
	slices.SortFunc(list.Items, func(a, b corev1.Event) int {
		return cmp.Compare(eventTimestamp(b).UnixNano(), eventTimestamp(a).UnixNano())
	})
	out := []activityItem{}
	for _, e := range list.Items {
		// Guard against a fake/permissive lister that ignores the selector.
		if e.InvolvedObject.Kind != kind || e.InvolvedObject.Name != name {
			continue
		}
		out = append(out, activityItem{
			Timestamp: eventTimestamp(e).UTC().Format("2006-01-02T15:04:05Z"),
			Namespace: e.InvolvedObject.Namespace,
			Kind:      e.InvolvedObject.Kind,
			Name:      e.InvolvedObject.Name,
			Reason:    e.Reason,
			Message:   e.Message,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}
