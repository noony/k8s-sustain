package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// windowPattern mirrors the CRD validator on Policy.Spec.RightSizing.ResourcesConfigs.*.Window
// exactly. Observation windows are coarse — sub-minute units are deliberately
// excluded. Reused for the chart window so the simulate API rejects junk
// strings before they hit Prometheus and either error out late or, worse,
// serialise into an unbounded query.
var windowPattern = regexp.MustCompile(`^([0-9]+(m|h|d|w|y))+$`)

// stepPattern allows sub-minute resolution (e.g. "30s", "5m") for the chart's
// data-point granularity. Distinct from windowPattern — chart resolution is a
// rendering concern, not a CRD field.
var stepPattern = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

// simulateRequest is the body accepted by POST /api/simulate: the identity,
// the range its charts span, and what to change of the settings the
// controller recommends it with.
type simulateRequest struct {
	Namespace string `json:"namespace"`
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
	Window    string `json:"window"`
	Step      string `json:"step"`
	FromTs    int64  `json:"fromTs,omitempty"`
	ToTs      int64  `json:"toTs,omitempty"`

	// Policy names the Policy the Simulation starts from; empty means the
	// identity's governing Policy.
	Policy string `json:"policy,omitempty"`
	// Override is a JSON merge patch (RFC 7386) of simulationSettings onto the
	// starting Policy's. Absent, the Simulation runs what the controller does.
	Override json.RawMessage `json:"override,omitempty"`
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req simulateRequest
	dec := json.NewDecoder(r.Body)
	// A body of the former shape would otherwise simulate the Policy unchanged.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
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
	if !slices.Contains(supportedWorkloadKinds, req.OwnerKind) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid ownerKind %q: must be one of %s", req.OwnerKind, strings.Join(supportedWorkloadKinds, ", ")))
		return
	}

	if req.Window == "" {
		req.Window = "168h"
	}
	if req.Step == "" {
		req.Step = "5m"
	}
	if !windowPattern.MatchString(req.Window) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid window %q: must be a duration like 168h, 14d (no sub-minute units)", req.Window))
		return
	}
	if !stepPattern.MatchString(req.Step) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid step %q: must be a Prometheus duration (e.g. 30s, 5m, 1h)", req.Step))
		return
	}

	if req.FromTs != 0 || req.ToTs != 0 {
		if req.FromTs == 0 || req.ToTs == 0 {
			writeError(w, http.StatusBadRequest, "fromTs and toTs must both be set")
			return
		}
		if perr := validateAbsoluteRange(req.FromTs, req.ToTs, time.Now()); perr != nil {
			writeError(w, http.StatusBadRequest, perr.Msg)
			return
		}
	}

	result, err := s.runSimulation(r.Context(), req)
	var invalid *settingsError
	switch {
	case errors.As(err, &invalid):
		writeFieldError(w, http.StatusBadRequest, "invalid override: "+invalid.Error(), invalid.field)
	case errors.Is(err, errBasePolicy):
		writeK8sGetError(w, err, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("simulation failed: %v", err))
	default:
		writeJSON(w, http.StatusOK, result)
	}
}
