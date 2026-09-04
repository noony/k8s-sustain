package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

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

// maxSummaryLastGoodAge bounds how stale the last-good /api/summary fallback
// may be when fresh queries partially fail; 10x the 60s cache TTL.
const maxSummaryLastGoodAge = 10 * time.Minute

// summaryCacheKey is the single cache and singleflight key for /api/summary.
const summaryCacheKey = "summary"

// summaryComputeTimeout bounds one shared recompute; it matches
// httpx.NewServer's WriteTimeout, past which no client is listening.
const summaryComputeTimeout = 15 * time.Second

// summaryComputation is what one shared recompute hands back to every request
// that joined it.
type summaryComputation struct {
	resp     summaryResponseV2
	promErrs int32
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	if cached, ok := s.summaryCache.Get(summaryCacheKey); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// Join the one in-flight recompute; one flight per key also guarantees a
	// single cache Set per recompute, so a slow one cannot overwrite a newer.
	ch := s.summarySF.DoChan(summaryCacheKey, s.computeAndCacheSummary)
	if s.sfJoinHook != nil {
		s.sfJoinHook(summaryCacheKey)
	}

	var res singleflight.Result
	select {
	case <-r.Context().Done():
		// Do not block past this request's own deadline waiting on the leader.
		if lastGood, ok := s.summaryCache.GetLastGoodWithin(summaryCacheKey, maxSummaryLastGoodAge); ok {
			writeJSON(w, http.StatusOK, lastGood)
			return
		}
		// Drop the payload's max-age so the 503 is not cached for a minute.
		w.Header().Del("Cache-Control")
		writeError(w, http.StatusServiceUnavailable, "summary recomputation did not complete before the request was cancelled")
		return
	case res = <-ch:
	}

	if res.Err != nil {
		// Only a recovered panic reaches here; re-panic on the request goroutine
		// so the recovery middleware handles it, and drop max-age first.
		w.Header().Del("Cache-Control")
		panic(res.Err)
	}
	out := res.Val.(summaryComputation)

	if out.promErrs == 0 {
		writeJSON(w, http.StatusOK, out.resp)
		return
	}

	// Partial failure: never poison the cache. Prefer a recent last-good
	// snapshot over the fresh-but-partial result.
	s.Logger.V(1).Info("summary: prometheus errors", "count", out.promErrs)
	if lastGood, ok := s.summaryCache.GetLastGoodWithin(summaryCacheKey, maxSummaryLastGoodAge); ok {
		writeJSON(w, http.StatusOK, lastGood)
		return
	}
	writeJSON(w, http.StatusOK, out.resp)
}

// computeAndCacheSummary is the singleflight leader. It runs on a detached
// context because other requests may still be waiting after the leading
// request is cancelled. The recover is load-bearing: a panic escaping a
// singleflight function with DoChan waiters takes the whole process down.
func (s *Server) computeAndCacheSummary() (val any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			val = nil
			err = fmt.Errorf("panic recomputing summary: %v\n%s", rec, debug.Stack())
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), summaryComputeTimeout)
	defer cancel()

	resp, promErrs := s.computeSummary(ctx)
	if promErrs == 0 {
		s.summaryCache.Set(summaryCacheKey, resp)
	}
	return summaryComputation{resp: resp, promErrs: promErrs}, nil
}

// computeSummary runs the Prometheus fan-out and assembles the response,
// returning it alongside the number of queries that failed.
func (s *Server) computeSummary(ctx context.Context) (summaryResponseV2, int32) {
	resp := summaryResponseV2{
		Headroom:  map[string]headroomBreakdown{},
		Attention: map[string][]attentionRow{"risk": {}, "drift": {}, "blocked": {}},
		Policies:  []policyRollup{},
	}

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
		// The OOM rule is per-container; re-aggregate to one row per workload.
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

	resp.Headroom["cpu"] = cpuHeadroom
	resp.Headroom["memory"] = memHeadroom
	resp.Attention["risk"] = riskRows
	resp.Attention["drift"] = driftRows
	resp.Attention["blocked"] = blockedRows

	for _, n := range atRiskByPolicy {
		resp.KPI.AtRiskCount += int(n)
	}

	// Union of policy keys so partial-data rollups still surface.
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

	return resp, promErrs
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
	// Key by the full namespace|kind|name triple so identically-named workloads
	// in different namespaces stay distinct.
	bySeries, err := p.QueryByLabels(ctx, expr, "namespace", "owner_kind", "owner_name")
	if err != nil {
		return rows, err
	}
	if len(bySeries) == 0 {
		return rows, nil
	}
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
