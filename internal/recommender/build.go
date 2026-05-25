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
// per-container recommendations for a single workload. The replica divisor
// is returned raw (MedianReplicas) so the caller can compute
// EffectiveReplicas with its autoscaler.Info — this lets autoscaler.Detect
// run in parallel with FetchWorkloadInputs.
type WorkloadInputs struct {
	CPUTotals      promclient.ContainerValues
	MemTotals      promclient.ContainerValues
	CPUFloors      promclient.ContainerValues // nil when per-pod floor query failed
	MemFloors      promclient.ContainerValues // nil when per-pod floor query failed
	MedianReplicas float64
	// OOM is the workload-level OOM signal from the past 24h. Empty when the
	// query failed (fail-open) — callers should not block recommendations on
	// missing OOM data.
	OOM promclient.OOMSignal
}

// HasRecentOOM reports whether the workload has any recent OOM activity in
// the Prometheus signal. Callers may OR this with their own in-process OOM
// observations (e.g. the controller's live OOM watcher) before deciding
// whether to bypass the workload-age gate.
func (w *WorkloadInputs) HasRecentOOM() bool {
	return w.OOM.OOMCount > 0
}

// FetchWorkloadInputs runs the Prometheus queries shared by the controller
// and webhook recommendation paths. All six queries run in parallel so the
// total wall time is bounded by the slowest single query rather than the sum
// — important on the webhook hot path. Per-pod floor and OOM query failures
// are non-fatal: a failure logs at V(1) and degrades to a nil/empty value so
// the workload-level result still produces a recommendation. CPU totals,
// memory totals, and replica count are fatal — they're the recommendation's
// primary inputs.
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
		cpuTotals, memTotals promclient.ContainerValues
		cpuFloors, memFloors promclient.ContainerValues
		oomSignal            promclient.OOMSignal
		median               float64
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		cpuTotals, err = pc.QueryWorkloadCPUByContainer(gctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
		if err != nil {
			return fmt.Errorf("workload cpu query: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		memTotals, err = pc.QueryWorkloadMemoryByContainer(gctx, ns, ownerKind, ownerName, memQuantile, memWindow)
		if err != nil {
			return fmt.Errorf("workload memory query: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		median, err = pc.QueryReplicaCountMedian(gctx, ns, ownerKind, ownerName, cpuWindow)
		if err != nil {
			return fmt.Errorf("replica count query: %w", err)
		}
		return nil
	})
	// Best-effort queries: swallow errors inside the goroutine so a floor or
	// OOM-signal failure doesn't cancel the errgroup.
	g.Go(func() error {
		v, err := pc.QueryCPUByContainer(gctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
		if err != nil {
			logger.V(1).Info("per-pod cpu floor query failed; proceeding without floor", "err", err)
			return nil
		}
		cpuFloors = v
		return nil
	})
	g.Go(func() error {
		v, err := pc.QueryMemoryByContainer(gctx, ns, ownerKind, ownerName, memQuantile, memWindow)
		if err != nil {
			logger.V(1).Info("per-pod memory floor query failed; proceeding without floor", "err", err)
			return nil
		}
		memFloors = v
		return nil
	})
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
		CPUTotals:      cpuTotals,
		MemTotals:      memTotals,
		CPUFloors:      cpuFloors,
		MemFloors:      memFloors,
		MedianReplicas: median,
		OOM:            oomSignal,
	}, nil
}

// ContainerInputs is the per-container slice of WorkloadInputs plus the
// OOM/autoscaler/config context needed to compute one recommendation.
type ContainerInputs struct {
	Container   corev1.Container
	CPUTotal    float64
	HasCPU      bool
	CPUFloor    float64
	MemTotal    float64
	HasMemUsage bool
	MemFloor    float64
	Replicas    float64
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

// ComputeContainerRec runs the shared per-container compute pipeline: CPU
// request, memory request (with optional OOM floor), autoscaler coordination,
// and limit derivation. HasData=false means neither CPU nor memory had enough
// signal to emit a recommendation — the caller should skip the container.
//
// Memory is emitted when EITHER usage samples are present OR the caller
// supplies an OOM context (recent OOM with peak data, or a live OOM event).
// This lets crash-looping containers — which can't accumulate usage samples
// — still receive a recommendation anchored on the kernel-observed peak.
func ComputeContainerRec(in ContainerInputs) ContainerRecResult {
	var rec workload.ContainerRecommendation
	hasData := false
	floorApplied := false

	if in.HasCPU {
		perPod := PerPodFromTotal(in.CPUTotal, in.Replicas)
		perPod = ApplyFloor(perPod, in.CPUFloor)
		rec.CPURequest = ComputeCPURequest(perPod, in.RsCfg.CPU.Requests)
		hasData = true
	}

	recent := in.OOM.Recent || !in.OOM.LiveEventAt.IsZero()
	hasLive := !in.OOM.LiveEventAt.IsZero()
	emitMem := in.HasMemUsage || (recent && in.HasOOMPeak) || hasLive
	if emitMem {
		var perPod float64
		if in.HasMemUsage {
			perPod = PerPodFromTotal(in.MemTotal, in.Replicas)
			perPod = ApplyFloor(perPod, in.MemFloor)
		}
		if cur := in.Container.Resources.Requests.Memory(); cur != nil && !cur.IsZero() {
			in.OOM.CurrentRequestBytes = float64(cur.Value())
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
