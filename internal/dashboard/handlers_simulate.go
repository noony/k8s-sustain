package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"k8s.io/apimachinery/pkg/api/resource"
)

// promDurationPattern matches the Prometheus duration grammar (e.g. "5m", "1h30m").
// Same regex used by the CRD validator on Policy.Spec...Window. Reused here so
// the simulate API rejects junk strings before they hit Prometheus and either
// error out late or, worse, serialise into an unbounded query.
var promDurationPattern = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

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

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	if !promDurationPattern.MatchString(req.Window) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid window %q: must be a Prometheus duration (e.g. 168h, 14d)", req.Window))
		return
	}
	if !promDurationPattern.MatchString(req.Step) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid step %q: must be a Prometheus duration (e.g. 5m, 1h)", req.Step))
		return
	}
	if err := validateSimulateResource(req.CPU, "cpu"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSimulateResource(req.Memory, "memory"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.runSimulation(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("simulation failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// validateSimulateResource bounds-checks user-supplied percentile/headroom
// (matching the CRD's accepted ranges) and rejects unparseable Quantity
// strings before they reach resource.MustParse — which would otherwise panic
// and surface as an unhelpful HTTP 500. Per-resource window is also validated.
func validateSimulateResource(cfg simulateResourceConfig, label string) error {
	if cfg.Percentile != nil {
		p := *cfg.Percentile
		if p < 1 || p > 100 {
			return fmt.Errorf("invalid %s percentile %d: must be 1..100", label, p)
		}
	}
	if cfg.Headroom != nil {
		h := *cfg.Headroom
		if h < 0 || h > 100 {
			return fmt.Errorf("invalid %s headroom %d: must be 0..100", label, h)
		}
	}
	if cfg.Window != "" && !promDurationPattern.MatchString(cfg.Window) {
		return fmt.Errorf("invalid %s window %q: must be a Prometheus duration", label, cfg.Window)
	}
	if cfg.MinAllowed != nil {
		if _, err := resource.ParseQuantity(*cfg.MinAllowed); err != nil {
			return fmt.Errorf("invalid %s minAllowed %q: %w", label, *cfg.MinAllowed, err)
		}
	}
	if cfg.MaxAllowed != nil {
		if _, err := resource.ParseQuantity(*cfg.MaxAllowed); err != nil {
			return fmt.Errorf("invalid %s maxAllowed %q: %w", label, *cfg.MaxAllowed, err)
		}
	}
	return nil
}
