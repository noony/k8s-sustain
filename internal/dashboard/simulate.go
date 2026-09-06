package dashboard

import (
	"cmp"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

type simulationResult struct {
	Containers         map[string]simulationContainerResult `json:"containers"`
	InitContainers     []string                             `json:"initContainers,omitempty"`
	TooYoung           bool                                 `json:"tooYoung,omitempty"`
	CPUSeries          promclient.ContainerTimeSeries       `json:"cpuSeries"`
	MemSeries          promclient.ContainerTimeSeries       `json:"memorySeries"`
	Resources          map[string]containerResources        `json:"resources,omitempty"`
	CPURequests        promclient.ContainerTimeSeries       `json:"cpuRequests,omitempty"`
	MemoryRequests     promclient.ContainerTimeSeries       `json:"memoryRequests,omitempty"`
	CPURecommendations promclient.ContainerTimeSeries       `json:"cpuRecommendations,omitempty"`
	MemRecommendations promclient.ContainerTimeSeries       `json:"memoryRecommendations,omitempty"`
}

type simulationContainerResult struct {
	CPURequest         string  `json:"cpuRequest"`
	MemoryRequest      string  `json:"memoryRequest"`
	CPULimit           string  `json:"cpuLimit,omitempty"`
	MemoryLimit        string  `json:"memoryLimit,omitempty"`
	CPULimitRemoved    bool    `json:"cpuLimitRemoved,omitempty"`
	MemoryLimitRemoved bool    `json:"memoryLimitRemoved,omitempty"`
	CPUUsageCores      float64 `json:"cpuUsageCores,omitempty"`
	MemoryUsageBytes   float64 `json:"memoryUsageBytes,omitempty"`
}

// simulationSpec is the resolved configuration one simulation runs with: the
// same shape a Policy provides, so the simulator and the controller feed the
// recommender identical inputs.
type simulationSpec struct {
	namespace, kind, name string
	// window and step shape the chart series only; the recommendation windows
	// live in resources.
	window, step string
	fromTs, toTs int64
	resources    sustainv1alpha1.ResourcesConfigs
	coordination sustainv1alpha1.AutoscalerCoordination
	excludeInit  bool
}

func (s simulationSpec) identity() promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: s.namespace, OwnerKind: s.kind, OwnerName: s.name}
}

// policySpec is the configuration the controller applies for this workload.
func policySpec(policy *sustainv1alpha1.Policy, namespace, kind, name string) simulationSpec {
	return simulationSpec{
		namespace:    namespace,
		kind:         kind,
		name:         name,
		resources:    policy.Spec.RightSizing.ResourcesConfigs,
		coordination: policy.Spec.RightSizing.AutoscalerCoordination,
		excludeInit:  policy.Spec.RightSizing.ExcludeInitContainers,
	}
}

// spec maps a user-supplied request onto the Policy shape. A resource's window
// falls back to the chart window; recommender.ResourceWindow supplies the
// final default.
func (req simulateRequest) spec(coordination sustainv1alpha1.AutoscalerCoordination) simulationSpec {
	return simulationSpec{
		namespace: req.Namespace,
		kind:      req.OwnerKind,
		name:      req.OwnerName,
		window:    req.Window,
		step:      req.Step,
		fromTs:    req.FromTs,
		toTs:      req.ToTs,
		resources: sustainv1alpha1.ResourcesConfigs{
			CPU: sustainv1alpha1.ResourceConfig{
				Window:   cmp.Or(req.CPU.Window, req.Window),
				Requests: buildRequestsConfig(req.CPU),
				Limits:   buildLimitsConfig(req.CPU.Limits),
			},
			Memory: sustainv1alpha1.ResourceConfig{
				Window:   cmp.Or(req.Memory.Window, req.Window),
				Requests: buildRequestsConfig(req.Memory),
				Limits:   buildLimitsConfig(req.Memory.Limits),
			},
		},
		coordination: coordination,
	}
}

// runSimulation resolves the workload and the coordination baseline, then
// runs the spec. Coordination defaults to the managing Policy's setting so an
// untouched simulation matches the recommendations endpoint; the request can
// override it.
func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	// A failed Get is tolerated: the zero entry yields nil resources and init
	// containers rather than failing the whole simulation.
	entry, err := s.getWorkloadEntry(ctx, req.Namespace, req.OwnerKind, req.OwnerName)
	if err != nil {
		s.Logger.Error(err, "failed to get workload entry", "namespace", req.Namespace, "kind", req.OwnerKind, "name", req.OwnerName)
		entry = workloadEntry{}
	}
	var coordination sustainv1alpha1.AutoscalerCoordination
	switch {
	case req.AutoscalerCoordination != nil:
		coordination = *req.AutoscalerCoordination
	default:
		if policy := s.managingPolicy(ctx, entry); policy != nil {
			coordination = policy.Spec.RightSizing.AutoscalerCoordination
		}
	}
	return s.runSimulationWithEntry(ctx, req.spec(coordination), entry)
}

// managingPolicy returns the Policy that manages entry, or nil when it is
// unmanaged or the Policies could not be listed.
func (s *Server) managingPolicy(ctx context.Context, entry workloadEntry) *sustainv1alpha1.Policy {
	policies, err := s.policiesByName(ctx)
	if err != nil {
		s.Logger.Error(err, "failed to list policies; simulating without the managing policy's autoscaler coordination")
		return nil
	}
	name, ok := resolveManagingPolicy(entry, policies, s.ExcludedNamespaces)
	if !ok {
		return nil
	}
	return policies[name]
}

// runSimulationWithEntry runs the spec against an already-fetched workload
// entry, avoiding a redundant API-server Get when the caller (e.g. the
// recommendations handler) has already resolved the workload.
func (s *Server) runSimulationWithEntry(ctx context.Context, spec simulationSpec, entry workloadEntry) (*simulationResult, error) {
	var tr promclient.TimeRange
	var err error
	if spec.fromTs > 0 && spec.toTs > 0 {
		tr = promclient.TimeRange{Start: time.Unix(spec.fromTs, 0), End: time.Unix(spec.toTs, 0)}
	} else {
		tr, err = promclient.TimeRangeFromWindow(recommender.ResourceWindow(spec.window), time.Now())
		if err != nil {
			return nil, fmt.Errorf("resolve chart window: %w", err)
		}
	}

	containers, _ := workload.MergeContainersForRecommendation(entry.Containers(), entry.InitContainers(), spec.excludeInit)
	autoInfo := s.autoscalerInfo(ctx, autoscaler.NewNamespacedSnapshot(s.K8sClient), spec)
	res, err := s.computeWorkloadRecs(ctx, spec, containers, entry.CreationTimestamp, autoInfo)
	if err != nil {
		return nil, err
	}

	step := cmp.Or(spec.step, "5m")
	ns, kind, name := spec.namespace, spec.kind, spec.name
	cpuSeries, err := s.PromClient.QueryCPURangeByContainer(ctx, ns, kind, name, tr, step)
	if err != nil {
		return nil, fmt.Errorf("cpu range query: %w", err)
	}
	memSeries, err := s.PromClient.QueryMemoryRangeByContainer(ctx, ns, kind, name, tr, step)
	if err != nil {
		return nil, fmt.Errorf("memory range query: %w", err)
	}
	cpuRequests, _ := s.PromClient.QueryCPURequestRangeByContainer(ctx, ns, kind, name, tr, step)
	memRequests, _ := s.PromClient.QueryMemoryRequestRangeByContainer(ctx, ns, kind, name, tr, step)

	cpuRecSeries, _ := s.PromClient.QueryWorkloadCPURecommendationRangeByContainer(ctx, ns, kind, name,
		recommender.PercentileQuantile(spec.resources.CPU.Requests.Percentile), recommender.ResourceWindow(spec.resources.CPU.Window), tr, step)
	memRecSeries, _ := s.PromClient.QueryWorkloadMemoryRecommendationRangeByContainer(ctx, ns, kind, name,
		recommender.PercentileQuantile(spec.resources.Memory.Requests.Percentile), recommender.ResourceWindow(spec.resources.Memory.Window), tr, step)

	return &simulationResult{
		Containers:         simulationContainers(res),
		InitContainers:     initContainerNamesFromEntry(entry),
		TooYoung:           res.TooYoung,
		CPUSeries:          cpuSeries,
		MemSeries:          memSeries,
		Resources:          containerResourcesFromEntry(entry),
		CPURequests:        cpuRequests,
		MemoryRequests:     memRequests,
		CPURecommendations: recommendationSeries(cpuRecSeries, spec, autoInfo, res.Inputs.OOM, false),
		MemRecommendations: recommendationSeries(memRecSeries, spec, autoInfo, res.Inputs.OOM, true),
	}, nil
}

// computeWorkloadRecs runs the shared recommendation algorithm for one
// workload under spec.
func (s *Server) computeWorkloadRecs(ctx context.Context, spec simulationSpec, containers []corev1.Container, created time.Time, autoInfo autoscaler.Info) (recommender.Result, error) {
	return recommender.Compute(ctx, s.PromClient, recommender.Request{
		Identity:        spec.identity(),
		Containers:      containers,
		Resources:       spec.resources,
		Coordination:    spec.coordination,
		AutoInfo:        autoInfo,
		WorkloadCreated: created,
	})
}

// autoscalerInfo detects the HPA or KEDA ScaledObject targeting the workload,
// the same lookup the controller's coordination uses. Detection failures
// degrade to "no autoscaler" so a missing RBAC rule cannot break the page.
func (s *Server) autoscalerInfo(ctx context.Context, snap *autoscaler.NamespacedSnapshot, spec simulationSpec) autoscaler.Info {
	info, err := snap.Lookup(ctx, spec.namespace, spec.kind, spec.name)
	if err != nil {
		s.Logger.V(1).Info("autoscaler detection failed; simulating without coordination",
			"namespace", spec.namespace, "kind", spec.kind, "name", spec.name, "err", err)
		return autoscaler.Info{Kind: autoscaler.KindNone}
	}
	return info
}

// simulationContainers renders the computed recommendations next to the raw
// usage they were derived from.
func simulationContainers(res recommender.Result) map[string]simulationContainerResult {
	out := make(map[string]simulationContainerResult, len(res.Recs))
	for name, rec := range res.Recs {
		cr := simulationContainerResult{
			CPUUsageCores:      res.Inputs.CPUPerPod[name],
			MemoryUsageBytes:   res.Inputs.MemPerPod[name],
			CPULimitRemoved:    rec.RemoveCPULimit,
			MemoryLimitRemoved: rec.RemoveMemoryLimit,
		}
		if rec.CPURequest != nil {
			cr.CPURequest = rec.CPURequest.String()
		}
		if rec.MemoryRequest != nil {
			cr.MemoryRequest = rec.MemoryRequest.String()
		}
		if rec.CPULimit != nil {
			cr.CPULimit = rec.CPULimit.String()
		}
		if rec.MemoryLimit != nil {
			cr.MemoryLimit = rec.MemoryLimit.String()
		}
		out[name] = cr
	}
	return out
}

// recommendationSeries runs each raw percentile point through the same
// per-container pipeline as the point value (headroom, min/max, OOM floor,
// autoscaler coordination), so the chart shows exactly what would be applied
// at each step. OOM recency is per container, so only the series of
// containers that actually OOMed get floored.
func recommendationSeries(series promclient.ContainerTimeSeries, spec simulationSpec, autoInfo autoscaler.Info, oom promclient.OOMSignal, memory bool) promclient.ContainerTimeSeries {
	if series == nil {
		return nil
	}
	out := make(promclient.ContainerTimeSeries, len(series))
	for name, points := range series {
		in := recommender.ContainerInputs{
			Container: corev1.Container{Name: name},
			AutoInfo:  autoInfo,
			RsCfg:     spec.resources,
			CoordCfg:  spec.coordination,
		}
		if memory {
			in.OOM = recommender.NewOOMSignal(oom.OOMCounts[name] > 0, oom.PeakMemoryBytes[name], oom.OOMLimitBytes[name])
			_, in.HasOOMPeak = oom.PeakMemoryBytes[name]
		}
		clamped := make([]promclient.TimeValue, len(points))
		for i, p := range points {
			if memory {
				in.MemPerPod, in.HasMemUsage = p.Value, true
			} else {
				in.CPUPerPod, in.HasCPU = p.Value, true
			}
			rec := recommender.ComputeContainerRec(in).Rec
			var v float64
			switch {
			case memory && rec.MemoryRequest != nil:
				v = float64(rec.MemoryRequest.Value())
			case !memory && rec.CPURequest != nil:
				v = float64(rec.CPURequest.MilliValue()) / 1000.0
			}
			clamped[i] = promclient.TimeValue{Timestamp: p.Timestamp, Value: v}
		}
		out[name] = clamped
	}
	return out
}

func buildRequestsConfig(cfg simulateResourceConfig) sustainv1alpha1.ResourceRequestsConfig {
	rc := sustainv1alpha1.ResourceRequestsConfig{
		Percentile: cfg.Percentile,
		Headroom:   cfg.Headroom,
	}
	// handleSimulate pre-validates these, so a parse failure is unreachable;
	// ParseQuantity rather than MustParse keeps a caller that skips the handler
	// from panicking into an HTTP 500.
	if cfg.MinAllowed != nil {
		if q, err := resource.ParseQuantity(*cfg.MinAllowed); err == nil {
			rc.MinAllowed = &q
		}
	}
	if cfg.MaxAllowed != nil {
		if q, err := resource.ParseQuantity(*cfg.MaxAllowed); err == nil {
			rc.MaxAllowed = &q
		}
	}
	return rc
}

func buildLimitsConfig(cfg *simulateLimitsConfig) sustainv1alpha1.ResourceLimitsConfig {
	if cfg == nil {
		return sustainv1alpha1.ResourceLimitsConfig{}
	}
	return sustainv1alpha1.ResourceLimitsConfig{
		EqualsToRequest:       cfg.EqualsToRequest,
		KeepLimit:             cfg.KeepLimit,
		KeepLimitRequestRatio: cfg.KeepLimitRequestRatio,
		NoLimit:               cfg.NoLimit,
		RequestsLimitsRatio:   cfg.RequestsLimitsRatio,
	}
}
