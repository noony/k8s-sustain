package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// simulateRequest is the body accepted by POST /api/simulate. It mirrors a
// subset of the Policy spec so users can preview how a configuration change
// would re-shape recommendations against the workload's historical signal.
type simulateRequest struct {
	Namespace string `json:"namespace"`
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
	Window    string `json:"window"`
	Step      string `json:"step"`

	CPU    simulateResourceConfig `json:"cpu"`
	Memory simulateResourceConfig `json:"memory"`
}

type simulateResourceConfig struct {
	Percentile *int32  `json:"percentile,omitempty"`
	Headroom   *int32  `json:"headroom,omitempty"`
	MinAllowed *string `json:"minAllowed,omitempty"`
	MaxAllowed *string `json:"maxAllowed,omitempty"`
	Window     string  `json:"window,omitempty"`
}

// handleSimulate validates the request, fills defaults, dispatches to
// runSimulation in simulate.go, and returns the JSON result.
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req simulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if req.Namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	if req.OwnerName == "" {
		writeError(w, http.StatusBadRequest, "ownerName is required")
		return
	}
	validKinds := map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "CronJob": true}
	if !validKinds[req.OwnerKind] {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid ownerKind %q: must be one of Deployment, StatefulSet, DaemonSet, CronJob", req.OwnerKind))
		return
	}

	if req.Window == "" {
		req.Window = "168h"
	}
	if req.Step == "" {
		req.Step = "5m"
	}

	result, err := s.runSimulation(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("simulation failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, result)
}
