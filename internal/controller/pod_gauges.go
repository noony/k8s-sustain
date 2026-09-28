package controller

import "sync"

type podGaugeKey struct {
	Namespace, Kind, Name string
}

// podGaugeTracker remembers each policy's last target set so the pod gauges of
// a workload that stops being a target are deleted instead of freezing at their
// last value. The gauges carry no policy label, so a key another policy still
// targets is left alone.
type podGaugeTracker struct {
	mu       sync.Mutex
	byPolicy map[string]map[podGaugeKey]struct{}
}

// observe records policy's current targets and deletes the series of those it
// no longer has. It must run before the policy emits for this cycle: a
// concurrent policy that already registered a moved key keeps its series.
func (tr *podGaugeTracker) observe(policy string, keys []podGaugeKey) {
	next := make(map[podGaugeKey]struct{}, len(keys))
	for _, k := range keys {
		next[k] = struct{}{}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.byPolicy == nil {
		tr.byPolicy = make(map[string]map[podGaugeKey]struct{})
	}
	prev := tr.byPolicy[policy]
	tr.byPolicy[policy] = next
	for k := range prev {
		if _, ok := next[k]; !ok {
			tr.deleteUnclaimedLocked(k)
		}
	}
}

func (tr *podGaugeTracker) forget(policy string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	prev := tr.byPolicy[policy]
	delete(tr.byPolicy, policy)
	for k := range prev {
		tr.deleteUnclaimedLocked(k)
	}
}

func (tr *podGaugeTracker) deleteUnclaimedLocked(k podGaugeKey) {
	for _, keys := range tr.byPolicy {
		if _, ok := keys[k]; ok {
			return
		}
	}
	DeleteWorkloadPods(k.Namespace, k.Kind, k.Name)
}
