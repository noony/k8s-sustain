package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// windowPattern mirrors the CRD validator on Policy.Spec.RightSizing.ResourcesConfigs.*.Window
// exactly. Observation windows are coarse — sub-minute units are deliberately
// excluded. Reused here so the simulate API rejects junk strings before they
// hit Prometheus and either error out late or, worse, serialise into an
// unbounded query.
var windowPattern = regexp.MustCompile(`^([0-9]+(m|h|d|w|y))+$`)

// stepPattern allows sub-minute resolution (e.g. "30s", "5m") for the chart's
// data-point granularity. Distinct from windowPattern — chart resolution is a
// rendering concern, not a CRD field.
var stepPattern = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

// simulateRequest is the body accepted by POST /api/simulate. It mirrors a
// subset of the Policy spec so users can preview how a configuration change
// would re-shape recommendations against the workload's historical signal.
type simulateRequest struct {
	Namespace string `json:"namespace"`
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
	Window    string `json:"window"`
	Step      string `json:"step"`
	FromTs    int64  `json:"fromTs,omitempty"`
	ToTs      int64  `json:"toTs,omitempty"`

	CPU    simulateResourceConfig `json:"cpu"`
	Memory simulateResourceConfig `json:"memory"`
}

type simulateResourceConfig struct {
	Percentile *int32                `json:"percentile,omitempty"`
	Headroom   *int32                `json:"headroom,omitempty"`
	MinAllowed *string               `json:"minAllowed,omitempty"`
	MaxAllowed *string               `json:"maxAllowed,omitempty"`
	Window     string                `json:"window,omitempty"`
	Limits     *simulateLimitsConfig `json:"limits,omitempty"`
}

type simulateLimitsConfig struct {
	EqualsToRequest       bool     `json:"equalsToRequest,omitempty"`
	KeepLimit             bool     `json:"keepLimit,omitempty"`
	KeepLimitRequestRatio bool     `json:"keepLimitRequestRatio,omitempty"`
	NoLimit               bool     `json:"noLimit,omitempty"`
	RequestsLimitsRatio   *float64 `json:"requestsLimitsRatio,omitempty"`
}

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
	if err := validateSimulateResource(req.CPU, "cpu"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSimulateResource(req.Memory, "memory"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	if cfg.Window != "" && !windowPattern.MatchString(cfg.Window) {
		return fmt.Errorf("invalid %s window %q: must be a duration like 168h, 14d (no sub-minute units)", label, cfg.Window)
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
	if cfg.Limits != nil {
		l := cfg.Limits
		n := 0
		if l.EqualsToRequest {
			n++
		}
		if l.KeepLimit {
			n++
		}
		if l.KeepLimitRequestRatio {
			n++
		}
		if l.NoLimit {
			n++
		}
		if l.RequestsLimitsRatio != nil {
			n++
			if *l.RequestsLimitsRatio < 1 {
				return fmt.Errorf("invalid %s requestsLimitsRatio %v: must be >= 1", label, *l.RequestsLimitsRatio)
			}
		}
		if n > 1 {
			return fmt.Errorf("invalid %s limits: at most one of equalsToRequest, keepLimit, keepLimitRequestRatio, noLimit, requestsLimitsRatio may be set", label)
		}
	}
	return nil
}
