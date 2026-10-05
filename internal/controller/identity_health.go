package controller

import (
	"slices"
	"sync"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// healthTracker turns the ledger's per-member state into one set of health
// series per identity, the key the dashboard and the recording rules join
// on. Members are applied concurrently and independently, so emitting from
// each member would be last-writer-wins; the tracker emits the identity's
// aggregate once its policy's pass ends.
//
// An identity is governed by at most one Policy at a time; members are still
// unioned across policies so that an identity moving between two keeps one
// aggregate, and its series are deleted, and its members forgotten by the
// ledger, only once no policy claims them.
type healthTracker struct {
	mu       sync.Mutex
	byPolicy map[string]map[promclient.WorkloadIdentity][]string
}

// observe records policy's live identities and their members, deleting the
// series of identities, and the ledger state of members, that no policy has
// any more. It must run before the policy emits for this cycle. A Conflicted
// identity is governed by no Policy, so it loses its series here: nothing
// applies to it.
func (h *healthTracker) observe(policy string, items []computeItem, l *ledger) {
	next := make(map[promclient.WorkloadIdentity][]string, len(items))
	for _, it := range items {
		if len(it.Targets) == 0 {
			continue
		}
		keys := make([]string, 0, len(it.Targets))
		for _, t := range it.Targets {
			keys = append(keys, t.key())
		}
		next[it.Identity] = keys
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byPolicy == nil {
		h.byPolicy = make(map[string]map[promclient.WorkloadIdentity][]string)
	}
	prev := h.byPolicy[policy]
	h.byPolicy[policy] = next
	h.dropUnclaimedLocked(prev, l)
}

func (h *healthTracker) forget(policy string, l *ledger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.byPolicy[policy]
	delete(h.byPolicy, policy)
	h.dropUnclaimedLocked(prev, l)
}

func (h *healthTracker) dropUnclaimedLocked(prev map[promclient.WorkloadIdentity][]string, l *ledger) {
	claimedIDs := make(map[promclient.WorkloadIdentity]struct{})
	claimedMembers := make(map[string]struct{})
	for _, ids := range h.byPolicy {
		for id, members := range ids {
			claimedIDs[id] = struct{}{}
			for _, m := range members {
				claimedMembers[m] = struct{}{}
			}
		}
	}
	for id, members := range prev {
		for _, m := range members {
			if _, ok := claimedMembers[m]; !ok {
				l.forget(m)
			}
		}
		if _, ok := claimedIDs[id]; !ok {
			DeleteIdentityHealth(id)
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

// emit writes the pod and retry-state series of every identity policy
// observed, each aggregated over all of the identity's members, and returns
// how many of those identities are Blocked.
//
// Pod counts are summed over the members that have been measured, so a
// member skipped this cycle (backoff, a failed pass) contributes its last
// counts. An identity is Blocked when any member is; the reason is the failed
// step of the first blocked member in sorted order.
func (h *healthTracker) emit(policy string, l *ledger) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	blocked := 0
	for id := range h.byPolicy[policy] {
		var sum workload.PodCounts
		measured := false
		reason := ""
		for _, m := range h.membersLocked(id) {
			mh := l.health(m)
			if mh.measured {
				sum.Total += mh.pods.Total
				sum.Stale += mh.pods.Stale
				measured = true
			}
			if reason == "" {
				reason = mh.blockedOn
			}
		}
		if measured {
			EmitWorkloadPods(id, sum)
		} else {
			DeleteWorkloadPods(id)
		}
		EmitRetryState(id, reason, reason != "")
		if reason != "" {
			blocked++
		}
	}
	return blocked
}
