package dashboard

import (
	"context"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// identityHealth is one identity's health signals. The zero value is an
// identity with no signal at all.
type identityHealth struct {
	OOM24h    int
	Blocked   *blockedSignal
	StalePods int
	TotalPods int
	// AutoscalerPresent and CoordinationFactors describe the autoscaler the
	// identity's recommendation is shaped against; CoordinationFactors is nil
	// without one.
	AutoscalerPresent   bool
	CoordinationFactors *coordinationFactors
}

type blockedSignal struct {
	// Reason is the reconcile step that failed: prometheus, patch or resize.
	Reason   string
	Attempts int
}

// HealthSignals is the port every view reads identity health through, so the
// views share one source and one classification (riskStateOf).
type HealthSignals interface {
	// forIdentities returns the health of each of ids that has any signal; an
	// identity missing from the result has none. A non-nil error comes with
	// whatever could still be read.
	forIdentities(ctx context.Context, ids []promclient.WorkloadIdentity) (map[promclient.WorkloadIdentity]identityHealth, error)
	// unhealthy returns every identity whose Risk state is not Safe.
	unhealthy(ctx context.Context) (map[promclient.WorkloadIdentity]identityHealth, error)
}

// riskState is an identity's Risk state, the single most actionable
// condition the dashboard shows for it.
type riskState string

const (
	riskSafe    riskState = "safe"
	riskDrifted riskState = "drifted"
	riskAtRisk  riskState = "at-risk"
	riskBlocked riskState = "blocked"
)

var riskStates = []string{string(riskSafe), string(riskDrifted), string(riskAtRisk), string(riskBlocked)}

// riskStateOf classifies one identity in the glossary's precedence: Blocked,
// then At risk (an OOM kill in the last 24h), then Drifted (stale pods), else
// Safe. Conflicted, which outranks them all, is not derived here yet.
func riskStateOf(h identityHealth) riskState {
	switch {
	case h.Blocked != nil:
		return riskBlocked
	case h.OOM24h > 0:
		return riskAtRisk
	case h.StalePods > 0:
		return riskDrifted
	default:
		return riskSafe
	}
}
