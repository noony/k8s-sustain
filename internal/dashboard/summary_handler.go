package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// ---- Summary types ----

// summaryKPI is the top-row of cluster-wide KPIs surfaced by /api/summary.
type summaryKPI struct {
	CPUSavedCores float64   `json:"cpuSavedCores"`
	CPUSavedRatio float64   `json:"cpuSavedRatio"`
	CPUSpark7d    []float64 `json:"cpuSpark7d"`
	MemSavedBytes float64   `json:"memSavedBytes"`
	MemSavedRatio float64   `json:"memSavedRatio"`
	MemSpark7d    []float64 `json:"memSpark7d"`
	AtRiskCount   int       `json:"atRiskCount"`
	DriftedCount  int       `json:"driftedCount"`
}

type headroomBreakdown struct {
	Used float64 `json:"used"`
	Idle float64 `json:"idle"`
	Free float64 `json:"free"`
}

type attentionRow struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Policy    string `json:"policy,omitempty"`
	Signal    string `json:"signal"`
	Detail    string `json:"detail,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
}

type policyRollup struct {
	Name            string  `json:"name"`
	WorkloadCount   int     `json:"workloadCount"`
	CPUSavingsCores float64 `json:"cpuSavingsCores"`
	MemSavingsBytes float64 `json:"memSavingsBytes"`
	AtRiskCount     int     `json:"atRiskCount"`
	LastAppliedAt   string  `json:"lastAppliedAt,omitempty"`
}

type summaryResponseV2 struct {
	KPI       summaryKPI                   `json:"kpi"`
	Headroom  map[string]headroomBreakdown `json:"headroom"`
	Attention map[string][]attentionRow    `json:"attention"`
	Policies  []policyRollup               `json:"policies"`
}

// maxSummaryLastGoodAge bounds how stale the last-good /api/summary fallback may
// be when fresh Prometheus queries partially fail. It is 10x the cache TTL (the
// summary cache uses a 60s TTL, so this is ~10 minutes). Within this window a
// brief Prometheus blip is masked by serving the most recent complete snapshot;
// beyond it we stop pretending the data is current and serve the fresh-but-
// partial result instead, so clients are never shown an arbitrarily-old snapshot
// as a clean HTTP 200 during a sustained outage.
const maxSummaryLastGoodAge = 10 * time.Minute

// ---- Summary handler ----

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	if cached, ok := s.summaryCache.Get("summary"); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	ctx := r.Context()
	resp := summaryResponseV2{
		Headroom:  map[string]headroomBreakdown{},
		Attention: map[string][]attentionRow{"risk": {}, "drift": {}, "blocked": {}},
		Policies:  []policyRollup{},
	}

	// Fan the independent Prometheus queries out concurrently so end-to-end
	// latency is bounded by the slowest single query, not their sum. Each
	// goroutine writes its own result slot; promErrs is aggregated atomically.
	// Mirrors the sync.WaitGroup / wg.Go idiom in handleWorkloadMetrics.
	var promErrs int32
	recordErr := func(err error) {
		if err != nil {
			atomic.AddInt32(&promErrs, 1)
		}
	}

	var (
		atRiskByPolicy           map[string]float64
		wlByPolicy               map[string]float64
		cpuByPolicy, memByPolicy map[string]float64
		cpuHeadroom, memHeadroom headroomBreakdown
		riskRows, driftRows      []attentionRow
		blockedRows              []attentionRow
	)

	var wg sync.WaitGroup
	wg.Go(func() {
		v, err := s.PromClient.QueryInstant(ctx, promclient.MetricClusterCPUSavingsCores)
		resp.KPI.CPUSavedCores = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryInstant(ctx, promclient.MetricClusterCPUSavingsRatio)
		resp.KPI.CPUSavedRatio = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryInstant(ctx, promclient.MetricClusterMemorySavingsBytes)
		resp.KPI.MemSavedBytes = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryInstant(ctx, promclient.MetricClusterMemorySavingsRatio)
		resp.KPI.MemSavedRatio = v
		recordErr(err)
	})
	wg.Go(func() {
		sparkTR, _ := promclient.TimeRangeFromWindow("168h", time.Now())
		v, err := sparklinePoints(ctx, s.PromClient, promclient.MetricClusterCPUSavingsCores, sparkTR, "30m")
		resp.KPI.CPUSpark7d = v
		recordErr(err)
	})
	wg.Go(func() {
		sparkTR, _ := promclient.TimeRangeFromWindow("168h", time.Now())
		v, err := sparklinePoints(ctx, s.PromClient, promclient.MetricClusterMemorySavingsBytes, sparkTR, "30m")
		resp.KPI.MemSpark7d = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyAtRiskCount, "policy")
		atRiskByPolicy = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryInstant(ctx, fmt.Sprintf("count(%s == 1)", promclient.MetricWorkloadDrifted))
		resp.KPI.DriftedCount = int(v)
		recordErr(err)
	})
	wg.Go(func() {
		v, err := readHeadroom(ctx, s.PromClient, promclient.MetricClusterCPUHeadroomBreakdown)
		cpuHeadroom = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := readHeadroom(ctx, s.PromClient, promclient.MetricClusterMemoryHeadroomBreakdown)
		memHeadroom = v
		recordErr(err)
	})
	wg.Go(func() {
		// The OOM rule is per-container; re-aggregate to workload level so
		// each at-risk workload yields exactly one attention row.
		v, err := collectAttention(ctx, s.PromClient, fmt.Sprintf("sum by (namespace, owner_kind, owner_name) (%s) > 0", promclient.MetricWorkloadOOM24h), "OOM")
		riskRows = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := collectAttention(ctx, s.PromClient, promclient.MetricWorkloadDrifted+" == 1", "drift")
		driftRows = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := collectAttention(ctx, s.PromClient, promclient.MetricWorkloadRetryState+" == 1", "blocked")
		blockedRows = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyWorkloadCount, "policy")
		wlByPolicy = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyCPUSavingsCores, "policy")
		cpuByPolicy = v
		recordErr(err)
	})
	wg.Go(func() {
		v, err := s.PromClient.QueryByLabel(ctx, promclient.MetricPolicyMemorySavingsBytes, "policy")
		memByPolicy = v
		recordErr(err)
	})
	wg.Wait()

	// Assemble shared maps single-threaded after the fan-out to avoid racing
	// concurrent writes to the same map.
	resp.Headroom["cpu"] = cpuHeadroom
	resp.Headroom["memory"] = memHeadroom
	resp.Attention["risk"] = riskRows
	resp.Attention["drift"] = driftRows
	resp.Attention["blocked"] = blockedRows

	// AtRiskCount aggregates the per-policy at-risk gauge produced above.
	for _, n := range atRiskByPolicy {
		resp.KPI.AtRiskCount += int(n)
	}

	// Iterate union of policy keys so partial-data rollups still surface.
	policyNames := make(map[string]struct{}, len(wlByPolicy)+len(cpuByPolicy)+len(memByPolicy))
	for n := range wlByPolicy {
		policyNames[n] = struct{}{}
	}
	for n := range cpuByPolicy {
		policyNames[n] = struct{}{}
	}
	for n := range memByPolicy {
		policyNames[n] = struct{}{}
	}
	sortedPolicyNames := slices.Sorted(maps.Keys(policyNames))
	for _, name := range sortedPolicyNames {
		resp.Policies = append(resp.Policies, policyRollup{
			Name:            name,
			WorkloadCount:   int(wlByPolicy[name]),
			CPUSavingsCores: cpuByPolicy[name],
			MemSavingsBytes: memByPolicy[name],
			AtRiskCount:     int(atRiskByPolicy[name]),
		})
	}

	if promErrs == 0 {
		// Full fresh result: cache it and serve it (unchanged behavior).
		s.summaryCache.Set("summary", resp)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Partial failure: never poison the cache. Prefer a recent last-good cached
	// value (even if its TTL has lapsed) over the fresh-but-partial result, but
	// only within maxSummaryLastGoodAge so a sustained outage cannot serve an
	// arbitrarily-old snapshot. When the last-good is too old or absent, fall
	// back to the fresh partial result, matching today's no-cache behavior.
	s.Logger.V(1).Info("summary: prometheus errors", "count", promErrs)
	if lastGood, ok := s.summaryCache.GetLastGoodWithin("summary", maxSummaryLastGoodAge); ok {
		writeJSON(w, http.StatusOK, lastGood)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func sparklinePoints(ctx context.Context, p PromQuerier, expr string, r promclient.TimeRange, step string) ([]float64, error) {
	pts, err := p.QueryRange(ctx, expr, r, step)
	if err != nil {
		return []float64{}, err
	}
	if len(pts) == 0 {
		return []float64{}, nil
	}
	out := make([]float64, 0, len(pts))
	for _, v := range pts {
		out = append(out, v.Value)
	}
	return out, nil
}

func readHeadroom(ctx context.Context, p PromQuerier, expr string) (headroomBreakdown, error) {
	bySeg, err := p.QueryByLabel(ctx, expr, "segment")
	if err != nil {
		return headroomBreakdown{}, err
	}
	return headroomBreakdown{Used: bySeg["used"], Idle: bySeg["idle"], Free: bySeg["free"]}, nil
}

func collectAttention(ctx context.Context, p PromQuerier, expr, signal string) ([]attentionRow, error) {
	rows := []attentionRow{}
	// Key by the full namespace|kind|name triple (the workloadKey format) so
	// identically-named workloads in different namespaces stay distinct and
	// each row carries everything the UI's /workloads/{ns}/{kind}/{name}
	// click-through link needs.
	bySeries, err := p.QueryByLabels(ctx, expr, "namespace", "owner_kind", "owner_name")
	if err != nil {
		return rows, err
	}
	if len(bySeries) == 0 {
		return rows, nil
	}
	// Sort deterministically: descending by value, ties broken alphabetically.
	keys := slices.Collect(maps.Keys(bySeries))
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(bySeries[b], bySeries[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	for _, key := range keys {
		ns, kind, name := splitWorkloadKey(key)
		rows = append(rows, attentionRow{Namespace: ns, Kind: kind, Name: name, Signal: signal})
	}
	return rows, nil
}
