package dashboard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// SignalQuerier is the part of the Prometheus client the health adapter uses.
type SignalQuerier interface {
	QueryByLabels(ctx context.Context, query string, labels ...string) (map[string]float64, error)
}

// NewPrometheusHealthSignals reads identity health from the controller's
// identity-keyed metrics and the OOM recording rule.
func NewPrometheusHealthSignals(q SignalQuerier) HealthSignals {
	return promHealthSignals{q: q}
}

type promHealthSignals struct {
	q SignalQuerier
}

func (p promHealthSignals) forIdentities(ctx context.Context, ids []promclient.WorkloadIdentity) (map[promclient.WorkloadIdentity]identityHealth, error) {
	out := make(map[promclient.WorkloadIdentity]identityHealth, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	sel := ""
	if len(ids) == 1 {
		sel = promclient.WorkloadSelector(ids[0].Namespace, ids[0].OwnerKind, ids[0].OwnerName)
	}
	all, err := p.read(ctx, sel)
	for _, id := range ids {
		if h, ok := all[id]; ok {
			out[id] = h
		}
	}
	return out, err
}

func (p promHealthSignals) unhealthy(ctx context.Context) (map[promclient.WorkloadIdentity]identityHealth, error) {
	all, err := p.read(ctx, "")
	for id, h := range all {
		if riskStateOf(h) == riskSafe {
			delete(all, id)
		}
	}
	return all, err
}

var identityLabels = []string{"namespace", "owner_kind", "owner_name"}

// read queries every signal, restricted by the series selector sel, and
// folds the results into one identityHealth per identity. Each query
// aggregates by identity so duplicate series (a second controller replica, a
// stale target) collapse into one.
func (p promHealthSignals) read(ctx context.Context, sel string) (map[promclient.WorkloadIdentity]identityHealth, error) {
	var (
		oom, stale, total, blocked, attempts, autoscaler map[string]float64

		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	query := func(dst *map[string]float64, agg, metric, filter string, extra ...string) {
		labels := append(slices.Clone(identityLabels), extra...)
		expr := fmt.Sprintf("%s by (%s) (%s%s%s)", agg, strings.Join(labels, ", "), metric, sel, filter)
		wg.Go(func() {
			v, err := p.q.QueryByLabels(ctx, expr, labels...)
			*dst = v
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	// The OOM rule is per container; summing keeps a 0-count sibling from
	// masking an OOMed one.
	query(&oom, "sum", promclient.MetricWorkloadOOM24h, "")
	query(&stale, "max", promclient.MetricWorkloadStalePods, "")
	query(&total, "max", promclient.MetricWorkloadPods, "")
	query(&blocked, "max", promclient.MetricWorkloadRetryState, " == 1", "reason")
	query(&attempts, "max", promclient.MetricWorkloadRetryAttempts, "")
	query(&autoscaler, "max", promclient.MetricAutoscalerPresent, "")
	wg.Wait()

	byID := map[promclient.WorkloadIdentity]*identityHealth{}
	each := func(m map[string]float64, extra int, fold func(h *identityHealth, rest []string, v float64)) {
		for key, v := range m {
			parts := strings.Split(key, "|")
			if len(parts) != len(identityLabels)+extra {
				continue
			}
			id := promclient.WorkloadIdentity{Namespace: parts[0], OwnerKind: parts[1], OwnerName: parts[2]}
			h, ok := byID[id]
			if !ok {
				h = &identityHealth{}
				byID[id] = h
			}
			fold(h, parts[len(identityLabels):], v)
		}
	}
	each(oom, 0, func(h *identityHealth, _ []string, v float64) { h.OOM24h = int(v) })
	each(stale, 0, func(h *identityHealth, _ []string, v float64) { h.StalePods = int(v) })
	each(total, 0, func(h *identityHealth, _ []string, v float64) { h.TotalPods = int(v) })
	each(blocked, 1, func(h *identityHealth, rest []string, _ float64) {
		h.Blocked = &blockedSignal{Reason: rest[0]}
	})
	each(autoscaler, 0, func(h *identityHealth, _ []string, v float64) {
		h.AutoscalerPresent = v > 0
	})

	out := make(map[promclient.WorkloadIdentity]identityHealth, len(byID))
	for id, h := range byID {
		if h.Blocked != nil {
			h.Blocked.Attempts = int(attempts[workloadKey(id.Namespace, id.OwnerKind, id.OwnerName)])
		}
		out[id] = *h
	}
	return out, errors.Join(errs...)
}
