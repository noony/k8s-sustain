package controller

import (
	"slices"
	"sync"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// healthReport is the health of every live identity a pass reported,
// aggregated over its members.
type healthReport map[promclient.WorkloadIdentity]healthState

// blocked counts the Blocked identities.
func (h healthReport) blocked() int {
	n := 0
	for _, s := range h {
		if s.blockedOn != "" {
			n++
		}
	}
	return n
}

// healthTracker turns the ledger's per-member state into one health per
// identity, the key the dashboard and the recording rules join on. Members
// are applied concurrently and independently, so the identity's health is
// aggregated once its policy's pass ends.
//
// An identity is governed by at most one Policy at a time; members are still
// unioned across policies so that an identity moving between two keeps one
// aggregate, and its members are forgotten by the ledger only once no policy
// claims them.
type healthTracker struct {
	mu       sync.Mutex
	byPolicy map[string]map[promclient.WorkloadIdentity][]string
}

func (h *healthTracker) forget(policy string, l *ledger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.byPolicy[policy]
	delete(h.byPolicy, policy)
	h.dropUnclaimedLocked(prev, l)
}

func (h *healthTracker) dropUnclaimedLocked(prev map[promclient.WorkloadIdentity][]string, l *ledger) {
	claimed := make(map[string]struct{})
	for _, ids := range h.byPolicy {
		for _, members := range ids {
			for _, m := range members {
				claimed[m] = struct{}{}
			}
		}
	}
	for _, members := range prev {
		for _, m := range members {
			if _, ok := claimed[m]; !ok {
				l.forget(m)
			}
		}
	}
}

// membersLocked returns every member any policy claims for id, sorted so the
// member a choice falls on does not depend on map order.
func (h *healthTracker) membersLocked(id promclient.WorkloadIdentity) []string {
	var out []string
	for _, ids := range h.byPolicy {
		for _, m := range ids[id] {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	slices.Sort(out)
	return out
}

// aggregate takes live, the keys of the members of every live identity
// policy governs, and returns each identity's health, aggregated over all of
// its members. The ledger forgets the members no policy claims any more.
//
// Pod counts are summed over the members that have been measured, so a
// member skipped this cycle (backoff, a failed pass) contributes its last
// counts. An identity is Blocked when any member is; the reason is the failed
// step of the first blocked member in sorted order.
func (h *healthTracker) aggregate(policy string, live map[promclient.WorkloadIdentity][]string, l *ledger) healthReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byPolicy == nil {
		h.byPolicy = make(map[string]map[promclient.WorkloadIdentity][]string)
	}
	prev := h.byPolicy[policy]
	h.byPolicy[policy] = live
	h.dropUnclaimedLocked(prev, l)
	out := make(healthReport, len(live))
	for id := range live {
		var s healthState
		for _, m := range h.membersLocked(id) {
			mh := l.health(m)
			if mh.measured {
				s.pods.Total += mh.pods.Total
				s.pods.Stale += mh.pods.Stale
				s.measured = true
			}
			if s.blockedOn == "" {
				s.blockedOn = mh.blockedOn
			}
		}
		out[id] = s
	}
	return out
}
