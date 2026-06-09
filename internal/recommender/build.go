package recommender

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"golang.org/x/sync/errgroup"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// MinWorkloadAge gates the first recommendation on a workload's age. The CPU
// rate rule needs a few minutes after container start to stabilize;
// recommending before that produces near-zero percentile values that get
// floored to the hard minimum and trigger an immediate recycle on the next
// reconcile. 10 minutes leaves headroom past the longest fallback window
// (5m) used by k8s_sustain:container_cpu_usage:rate1m.
//
// A recent OOM event bypasses the gate so crash-looping workloads can still
// receive a memory recommendation anchored on the OOM peak.
const MinWorkloadAge = 10 * time.Minute

// ShouldSkipYoungWorkload reports whether a workload is too young to have
// produced stable rate samples and has no recent OOM to bypass the gate.
// Returns false when workloadCreated is zero (unknown / age-gating disabled).
func ShouldSkipYoungWorkload(workloadCreated time.Time, recentOOM bool) bool {
	if recentOOM || workloadCreated.IsZero() {
		return false
	}
	return time.Since(workloadCreated) < MinWorkloadAge
}

// WorkloadInputs bundles the Prometheus query results needed to build
// per-container recommendations for a single workload. CPUPerPod and MemPerPod
// are already per-pod percentiles of the busiest replica (see
// QueryWorkloadCPUByContainer) — no replica division is applied downstream.
type WorkloadInputs struct {
	CPUPerPod promclient.ContainerValues
	MemPerPod promclient.ContainerValues
	// OOM is the workload's OOM signal from the past 24h, with per-container
	// OOM counts. Empty when the query failed (fail-open) — callers should
	// not block recommendations on missing OOM data.
	OOM promclient.OOMSignal
}

// HasRecentOOM reports whether the workload has any recent OOM activity in
// the Prometheus signal, in ANY container. Callers may OR this with their own
// in-process OOM observations (e.g. the controller's live OOM watcher) before
// deciding whether to bypass the workload-age gate — a workload that OOMed
// anywhere is not "too young to have data". Per-container OOM recency (which
// drives the memory floor) is derived from OOM.OOMCounts in BuildContainerRecs
// instead.
func (w *WorkloadInputs) HasRecentOOM() bool {
	return w.OOM.TotalOOMs() > 0
}

// FetchWorkloadInputs runs the Prometheus queries shared by the controller
// and webhook recommendation paths. All three queries run in parallel so the
// total wall time is bounded by the slowest single query rather than the sum
// — important on the webhook hot path. The OOM-signal failure is non-fatal: it
// logs at V(1) and degrades to an empty value so the workload still produces a
// recommendation. The CPU and memory per-pod percentiles are fatal — they're
// the recommendation's primary inputs.
func FetchWorkloadInputs(
	ctx context.Context,
	pc *promclient.Client,
	ns, ownerKind, ownerName string,
	rsCfg sustainv1alpha1.ResourcesConfigs,
) (*WorkloadInputs, error) {
	cpuQuantile := PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := ResourceWindow(rsCfg.CPU.Window)
	memQuantile := PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := ResourceWindow(rsCfg.Memory.Window)

	logger := log.FromContext(ctx)

	var (
		cpuPerPod, memPerPod promclient.ContainerValues
		oomSignal            promclient.OOMSignal
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		cpuPerPod, err = pc.QueryWorkloadCPUByContainer(gctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
		if err != nil {
			return fmt.Errorf("workload cpu query: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		memPerPod, err = pc.QueryWorkloadMemoryByContainer(gctx, ns, ownerKind, ownerName, memQuantile, memWindow)
		if err != nil {
			return fmt.Errorf("workload memory query: %w", err)
		}
		return nil
	})
	// Best-effort query: swallow the error inside the goroutine so an
	// OOM-signal failure doesn't cancel the errgroup.
	g.Go(func() error {
		v, err := pc.QueryWorkloadOOMSignal(gctx, ns, ownerKind, ownerName)
		if err != nil {
			logger.V(1).Info("oom signal query failed; proceeding without OOM floor", "err", err)
			return nil
		}
		oomSignal = v
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return &WorkloadInputs{
		CPUPerPod: cpuPerPod,
		MemPerPod: memPerPod,
		OOM:       oomSignal,
	}, nil
}

// ContainerInputs is the per-container slice of WorkloadInputs plus the
// OOM/autoscaler/config context needed to compute one recommendation. CPUPerPod
// and MemPerPod are already per-pod percentiles (busiest replica) — they feed
// the request computation directly without replica division.
type ContainerInputs struct {
	Container   corev1.Container
	CPUPerPod   float64
	HasCPU      bool
	MemPerPod   float64
	HasMemUsage bool
	// OOM is the per-container memory floor signal. Pass the zero value when
	// the caller has no OOM context (the webhook path). HasOOMPeak gates
	// memory emission when usage samples are absent — see ComputeContainerRec.
	OOM        OOMSignal
	HasOOMPeak bool
	AutoInfo   autoscaler.Info
	RsCfg      sustainv1alpha1.ResourcesConfigs
	CoordCfg   sustainv1alpha1.AutoscalerCoordination
}

// ContainerRecResult is the output of ComputeContainerRec. Base holds the
// pre-coordination recommendation so the caller can emit coordination-factor
// metrics by comparing Base vs Rec.
type ContainerRecResult struct {
	Rec             workload.ContainerRecommendation
	Base            workload.ContainerRecommendation
	HasData         bool
	MemFloorApplied bool
}

// BuildContainerRecsOptions carries the optional per-container hooks for
// BuildContainerRecs. Both hooks are optional (nil-safe) so the webhook can
// pass the zero value while the controller injects its live-OOM merge and
// metric emission.
type BuildContainerRecsOptions struct {
	// EnrichOOM, when non-nil, is called for every container after the base
	// OOMSignal is constructed (via NewOOMSignal) but before ComputeContainerRec
	// runs. It returns the OOMSignal to actually compute with. The controller
	// uses this to fold in its in-memory live-OOM observation. The webhook
	// leaves it nil and the base signal is used unchanged.
	EnrichOOM func(name string, sig OOMSignal) OOMSignal
	// OnResult, when non-nil, is called for every container whose recommendation
	// has data (res.HasData), after ComputeContainerRec. The controller uses
	// this to emit per-container metrics (OOM floor, reaction latency,
	// coordination factors). It runs before the result is stored in the
	// returned map; mutating res.Rec there is not supported — store happens from
	// the value returned by ComputeContainerRec.
	OnResult func(name string, res ContainerRecResult)
}

// BuildContainerRecs runs the per-container recommendation loop shared by the
// controller and the webhook. For each container it slices the per-pod inputs,
// builds the OOM signal (NewOOMSignal, optionally enriched via
// opts.EnrichOOM), computes the recommendation (ComputeContainerRec), and
// collects the results with HasData. Each container's OOM recency is derived
// from inputs.OOM.OOMCounts — only containers that actually OOMed get the
// memory floor, never innocent siblings. Callers fold their own per-container
// OOM observations in via opts.EnrichOOM (the controller stamps LiveEventAt
// for containers with a live OOM record, which is treated as recent).
//
// This is the single source of truth for skip/threshold semantics on both
// injection paths — the workload-age gate (ShouldSkipYoungWorkload) is applied
// by the caller, but the per-container HasData handling lives here so the two
// paths cannot drift.
func BuildContainerRecs(
	containers []corev1.Container,
	inputs *WorkloadInputs,
	autoInfo autoscaler.Info,
	rsCfg sustainv1alpha1.ResourcesConfigs,
	coordCfg sustainv1alpha1.AutoscalerCoordination,
	opts BuildContainerRecsOptions,
) map[string]workload.ContainerRecommendation {
	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		cpuPerPod, hasCPU := inputs.CPUPerPod[c.Name]
		memPerPod, hasMem := inputs.MemPerPod[c.Name]
		_, hasPeak := inputs.OOM.PeakMemoryBytes[c.Name]

		recentOOM := inputs.OOM.OOMCounts[c.Name] > 0
		oom := NewOOMSignal(recentOOM, inputs.OOM.PeakMemoryBytes[c.Name], inputs.OOM.OOMLimitBytes[c.Name])
		if opts.EnrichOOM != nil {
			oom = opts.EnrichOOM(c.Name, oom)
		}

		res := ComputeContainerRec(ContainerInputs{
			Container:   c,
			CPUPerPod:   cpuPerPod,
			HasCPU:      hasCPU,
			MemPerPod:   memPerPod,
			HasMemUsage: hasMem,
			OOM:         oom,
			HasOOMPeak:  hasPeak,
			AutoInfo:    autoInfo,
			RsCfg:       rsCfg,
			CoordCfg:    coordCfg,
		})
		if !res.HasData {
			continue
		}
		if opts.OnResult != nil {
			opts.OnResult(c.Name, res)
		}
		recs[c.Name] = res.Rec
	}
	return recs
}

// ComputeContainerRec runs the shared per-container compute pipeline: CPU
// request, memory request (with optional OOM floor), autoscaler coordination,
// and limit derivation. HasData=false means neither CPU nor memory had enough
// signal to emit a recommendation — the caller should skip the container.
//
// Memory is emitted when EITHER usage samples are present OR a recent/live
// OOM comes with a positive anchor (kernel-observed peak or OOM-time limit).
// This lets crash-looping containers — which can't accumulate usage samples
// — still receive a recommendation anchored on real data. An OOM event with
// no anchor at all emits nothing: the only possible output would be the hard
// 1Mi minimum, which would guarantee the next OOM.
func ComputeContainerRec(in ContainerInputs) ContainerRecResult {
	var rec workload.ContainerRecommendation
	hasData := false
	floorApplied := false

	if in.HasCPU {
		rec.CPURequest = ComputeCPURequest(in.CPUPerPod, in.RsCfg.CPU.Requests)
		hasData = true
	}

	recent := in.OOM.Recent || !in.OOM.LiveEventAt.IsZero()
	emitMem := in.HasMemUsage || (recent && (in.HasOOMPeak || in.OOM.OOMTimeLimitBytes > 0))
	if emitMem {
		var perPod float64
		if in.HasMemUsage {
			perPod = in.MemPerPod
		}
		rec.MemoryRequest, floorApplied = ComputeMemoryRequestWithOOMFloorReport(perPod, in.OOM, in.RsCfg.Memory.Requests)
		hasData = true
	}

	if !hasData {
		return ContainerRecResult{}
	}

	base := rec
	rec = ApplyCoordination(rec, in.CoordCfg, in.AutoInfo, in.RsCfg)

	if rec.CPURequest != nil {
		lr := ComputeLimit(rec.CPURequest, in.Container.Resources.Requests.Cpu(), in.Container.Resources.Limits.Cpu(), in.RsCfg.CPU.Limits)
		rec.CPULimit = lr.Quantity
		rec.RemoveCPULimit = lr.Remove
	}
	if rec.MemoryRequest != nil {
		lr := ComputeLimit(rec.MemoryRequest, in.Container.Resources.Requests.Memory(), in.Container.Resources.Limits.Memory(), in.RsCfg.Memory.Limits)
		rec.MemoryLimit = lr.Quantity
		rec.RemoveMemoryLimit = lr.Remove
	}

	return ContainerRecResult{
		Rec:             rec,
		Base:            base,
		HasData:         true,
		MemFloorApplied: floorApplied,
	}
}
