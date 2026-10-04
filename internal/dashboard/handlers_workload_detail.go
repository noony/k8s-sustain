package dashboard

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

type workloadDetailResponse struct {
	// Automated is set when a Policy governs the identity; PolicyName names it.
	Automated  bool   `json:"automated"`
	PolicyName string `json:"policyName,omitempty"`
	// ConflictingPolicies names the Policies a Conflicted identity's members
	// opt into.
	ConflictingPolicies []string               `json:"conflictingPolicies,omitempty"`
	Departed            bool                   `json:"departed"`
	UpdateMode          string                 `json:"updateMode,omitempty"`
	RiskState           riskState              `json:"riskState"`
	StalePods           int                    `json:"stalePods"`
	TotalPods           int                    `json:"totalPods"`
	OOM24h              int                    `json:"oom24h"`
	Blocked             *workloadDetailBlocked `json:"blocked,omitempty"`
	RecentEvents        []activityItem         `json:"recentEvents"`
	CoordinationFactors *coordinationFactors   `json:"coordinationFactors,omitempty"`
	// Recommendation is what the identity's WorkloadRecommendation holds, nil
	// until it has one. It is never recomputed here: that is a Simulation.
	Recommendation *storedRecommendation `json:"recommendation,omitempty"`
}

type workloadDetailBlocked struct {
	Reason      string `json:"reason"`
	Attempts    int    `json:"attempts"`
	NextRetryAt string `json:"nextRetryAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

// storedRecommendation is a WorkloadRecommendation's status as the detail page
// shows it.
type storedRecommendation struct {
	Outcome    sustainv1alpha1.RecommendationOutcome `json:"outcome,omitempty"`
	ObservedAt string                                `json:"observedAt,omitempty"`
	Containers map[string]simulationContainerResult  `json:"containers,omitempty"`
	// Trace is how each container's values were derived, stage by stage.
	Trace map[string]sustainv1alpha1.ContainerTrace `json:"trace,omitempty"`
}

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	key := promclient.WorkloadIdentity{Namespace: namespace, OwnerKind: kind, OwnerName: name}
	id, err := s.identity(ctx, key)
	if err != nil {
		writeIdentityError(w, key, err)
		return
	}

	resp := workloadDetailResponse{
		Automated:           id.Policy != "",
		PolicyName:          id.Policy,
		ConflictingPolicies: conflictingPolicies(id),
		Departed:            id.Departed(),
		CoordinationFactors: coordinationFactorsOf(id.Recommendation),
		Recommendation:      storedRecommendationOf(id.Recommendation),
	}
	if policy := s.governingPolicy(ctx, id); policy != nil {
		if mode := policy.Spec.RightSizing.Update.Types.ModeForKind(kind); mode != nil {
			resp.UpdateMode = string(*mode)
		}
	}
	s.fillDetailHealth(ctx, &resp, id)
	resp.RecentEvents = s.recentSustainEvents(ctx, id, 10)

	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, resp)
}

func storedRecommendationOf(wlr *sustainv1alpha1.WorkloadRecommendation) *storedRecommendation {
	if wlr == nil {
		return nil
	}
	out := &storedRecommendation{Outcome: wlr.Status.Outcome, Trace: wlr.Status.Trace}
	if !wlr.Status.ObservedAt.IsZero() {
		out.ObservedAt = wlr.Status.ObservedAt.UTC().Format(time.RFC3339)
	}
	if len(wlr.Status.Containers) > 0 {
		out.Containers = make(map[string]simulationContainerResult, len(wlr.Status.Containers))
	}
	for name, rec := range wlr.Status.Containers {
		c := simulationContainerResult{CPULimitRemoved: rec.RemoveCPULimit, MemoryLimitRemoved: rec.RemoveMemoryLimit}
		if rec.CPURequest != nil {
			c.CPURequest = rec.CPURequest.String()
		}
		if rec.MemoryRequest != nil {
			c.MemoryRequest = rec.MemoryRequest.String()
		}
		if rec.CPULimit != nil {
			c.CPULimit = rec.CPULimit.String()
		}
		if rec.MemoryLimit != nil {
			c.MemoryLimit = rec.MemoryLimit.String()
		}
		out.Containers[name] = c
	}
	return out
}

// fillDetailHealth overlays the identity's health and Risk state; a failed
// read degrades to the signals that could be read.
func (s *Server) fillDetailHealth(ctx context.Context, resp *workloadDetailResponse, id *inventory.Identity) {
	health, err := s.Health.forIdentities(ctx, []promclient.WorkloadIdentity{id.Key})
	if err != nil {
		s.Logger.V(1).Info("reading identity health failed; detail shows what could be read", "error", err.Error())
	}
	h := health[id.Key]
	h.Conflicted = id.Conflicted
	resp.RiskState = riskStateOf(h)
	resp.OOM24h = h.OOM24h
	resp.StalePods = h.StalePods
	resp.TotalPods = h.TotalPods
	if h.Blocked != nil {
		resp.Blocked = &workloadDetailBlocked{Reason: h.Blocked.Reason, Attempts: h.Blocked.Attempts}
	}
}

// recentSustainEvents returns up to limit k8s-sustain events about any member
// of the identity, most recent first. The controller attributes events to the
// member it acted on, so an owner-name group's events sit on several objects
// and a bare-pod identity's on its pods. A Departed identity has no member
// left; its events may still name the identity itself.
func (s *Server) recentSustainEvents(ctx context.Context, id *inventory.Identity, limit int) []activityItem {
	names := map[string]bool{}
	for _, m := range id.Members {
		names[m.Object.GetName()] = true
	}
	if len(names) == 0 {
		names[id.Key.OwnerName] = true
	}
	// Narrowed server-side through the built-in involvedObject.* field
	// selectors, so the activityListLimit cap cannot truncate this identity's
	// events the way a namespace-wide backlog could. A field selector cannot
	// OR names, so several members are matched by kind and filtered here.
	fields := client.MatchingFields{"source": "k8s-sustain", "involvedObject.kind": id.Key.OwnerKind}
	if len(names) == 1 {
		for name := range names {
			fields["involvedObject.name"] = name
		}
	}
	var list corev1.EventList
	_ = s.K8sClient.List(ctx, &list, client.InNamespace(id.Key.Namespace), client.Limit(activityListLimit), fields)
	// The API server does not guarantee Events come back ordered by recency, so
	// sort newest-first before applying the keep-limit cap.
	slices.SortFunc(list.Items, func(a, b corev1.Event) int {
		return cmp.Compare(eventTimestamp(b).UnixNano(), eventTimestamp(a).UnixNano())
	})
	out := []activityItem{}
	for _, e := range list.Items {
		if e.InvolvedObject.Kind != id.Key.OwnerKind || !names[e.InvolvedObject.Name] {
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
