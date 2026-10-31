package dashboard

import (
	"context"

	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// identityStatus is one identity as every view shows it: its health signals,
// the Risk state they classify to, and the autoscaler coordination factors of
// its stored Recommendation (nil when coordination did not shape it).
type identityStatus struct {
	id           *inventory.Identity
	health       identityHealth
	risk         riskState
	coordination *coordinationFactors
}

// board reads the health of ids in one batch and classifies each identity.
// Every view is built from a board, so the Overview, the lists and the detail
// page agree on each identity's Risk state. A failed health read is logged and
// comes back with the statuses of the signals that could be read; the error is
// returned for callers that must not cache such a board.
func (s *Server) board(ctx context.Context, ids []*inventory.Identity) ([]identityStatus, error) {
	keys := make([]promclient.WorkloadIdentity, len(ids))
	for i, id := range ids {
		keys[i] = id.Key
	}
	health, err := s.Health.forIdentities(ctx, keys)
	if err != nil {
		s.Logger.V(1).Info("reading identity health failed; statuses show what could be read", "error", err.Error())
	}
	out := make([]identityStatus, len(ids))
	for i, id := range ids {
		h := health[id.Key]
		h.Conflicted = id.Conflicted
		out[i] = identityStatus{
			id:           id,
			health:       h,
			risk:         riskStateOf(h),
			coordination: coordinationFactorsOf(id.Recommendation),
		}
	}
	return out, err
}

// clusterIdentities is every identity the Workloads list shows. The Overview
// reads the same ones, so each of its counts equals that list filtered by the
// same Risk state.
func (s *Server) clusterIdentities(ctx context.Context) ([]*inventory.Identity, error) {
	snap, err := s.identities(ctx, inventory.Options{})
	if err != nil {
		return nil, err
	}
	ids := make([]*inventory.Identity, len(snap.Identities))
	for i := range snap.Identities {
		ids[i] = &snap.Identities[i]
	}
	return ids, nil
}
