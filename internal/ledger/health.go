package ledger

import (
	"cmp"
	"slices"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// claimLocked makes claims what policy claims, nil for nothing, and forgets
// the members policy claimed before that no Policy claims any more.
func (l *Ledger) claimLocked(policy string, claims map[promclient.WorkloadIdentity][]MemberKey) {
	prev := l.claims[policy]
	if claims == nil {
		delete(l.claims, policy)
	} else {
		l.claims[policy] = claims
	}
	claimed := make(map[MemberKey]struct{})
	for _, ids := range l.claims {
		for _, members := range ids {
			for _, m := range members {
				claimed[m] = struct{}{}
			}
		}
	}
	for _, members := range prev {
		for _, m := range members {
			if _, ok := claimed[m]; !ok {
				delete(l.members, m)
			}
		}
	}
}

// healthLocked is id's health over every member any Policy claims for it.
func (l *Ledger) healthLocked(id promclient.WorkloadIdentity) Health {
	var h Health
	for _, m := range l.claimedLocked(id) {
		e := l.members[m]
		if e == nil {
			continue
		}
		if e.measured {
			h.Pods.Total += e.pods.Total
			h.Pods.Stale += e.pods.Stale
			h.Measured = true
		}
		if h.BlockedOn == "" && e.failure != nil {
			h.BlockedOn = e.failure.step
		}
	}
	return h
}

// claimedLocked returns every member any Policy claims for id, sorted so the
// member a choice falls on does not depend on map order.
func (l *Ledger) claimedLocked(id promclient.WorkloadIdentity) []MemberKey {
	var out []MemberKey
	for _, ids := range l.claims {
		for _, m := range ids[id] {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	slices.SortFunc(out, func(a, b MemberKey) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}
