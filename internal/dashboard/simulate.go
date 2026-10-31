package dashboard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

type simulationResult struct {
	// Policy is the Policy the Simulation started from, empty when the
	// identity has no governing Policy and none was chosen.
	Policy string `json:"policy,omitempty"`
	// Effective is what it ran with: the Policy's settings with the override
	// merged in and every default explicit.
	Effective      simulationSettings                        `json:"effective"`
	Outcome        sustainv1alpha1.RecommendationOutcome     `json:"outcome"`
	Containers     map[string]simulationContainerResult      `json:"containers"`
	Trace          map[string]sustainv1alpha1.ContainerTrace `json:"trace,omitempty"`
	InitContainers []string                                  `json:"initContainers,omitempty"`
	Resources      map[string]containerResources             `json:"resources,omitempty"`
	// History is what the Simulation computes at each instant of its chart
	// range, nil when it could not be read.
	History *simulationHistory `json:"history,omitempty"`
	// HistoryError is why the history could not be read. The Simulation's
	// numbers stand without it.
	HistoryError string `json:"historyError,omitempty"`
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

// simulationHistory is a Simulation at each instant of its chart range.
type simulationHistory struct {
	// StepSeconds is the step between its instants, derived from the range:
	// the step a request asks for is only the finest it gets.
	StepSeconds int64                       `json:"stepSeconds"`
	Containers  map[string]containerHistory `json:"containers"`
	// OOMKills are the identity's OOM kills over the range, each dated at the
	// end of the step it fell in.
	OOMKills []promclient.OOMKill `json:"oomKills,omitempty"`
}

type containerHistory struct {
	CPU    resourceHistory `json:"cpu"`
	Memory resourceHistory `json:"memory"`
}

// resourceHistory is one resource of one container at each instant, in cores
// or bytes. A series has no point where it has no value: no recommended
// request where none is recommended, no limit where the container would run
// without one.
type resourceHistory struct {
	// Usage is what the resource's usage signal read: the busiest replica's
	// percentile over the Policy's window.
	Usage []promclient.TimeValue `json:"usage,omitempty"`
	// RecommendedRequest and RecommendedLimit are what the Simulation computes
	// as of each instant.
	RecommendedRequest []promclient.TimeValue `json:"recommendedRequest,omitempty"`
	RecommendedLimit   []promclient.TimeValue `json:"recommendedLimit,omitempty"`
	// RunningRequest and RunningLimit are what the pods ran with.
	RunningRequest []promclient.TimeValue `json:"runningRequest,omitempty"`
	RunningLimit   []promclient.TimeValue `json:"runningLimit,omitempty"`
}

var errBasePolicy = errors.New("the Policy to simulate")

// runSimulation computes the Request the controller would make of the
// identity, under the starting Policy's settings with the override merged in:
// with no override and the governing Policy, exactly the controller's, live
// OOM kills aside. An identity the inventory does not hold is simulated from
// whatever Prometheus reports for it. Its history over the chart range is
// computed by the same chain, and a history that cannot be read leaves the
// numbers standing with the reason.
func (s *Server) runSimulation(ctx context.Context, req simulateRequest) (*simulationResult, error) {
	key := promclient.WorkloadIdentity{Namespace: req.Namespace, OwnerKind: req.OwnerKind, OwnerName: req.OwnerName}
	id, err := s.identity(ctx, key)
	switch {
	case apierrors.IsNotFound(err):
		id = &inventory.Identity{Key: key}
	case err != nil:
		return nil, fmt.Errorf("reading the workload identity: %w", err)
	}
	policy := cmp.Or(req.Policy, id.Policy)
	base, err := s.policySettings(ctx, policy)
	if err != nil {
		return nil, err
	}
	settings, err := base.override(req.Override)
	if err != nil {
		return nil, err
	}

	a := recommender.Assemble(ctx, s.Inputs, autoscaler.NewNamespacedSnapshot(s.K8sClient), s.now, settings.rightSizing(), []*inventory.Identity{id})[0]
	if a.Err != nil {
		return nil, a.Err
	}
	res := recommender.Compute(a.Request)
	settings.ResourcesConfigs = a.Request.Resources()
	out := &simulationResult{
		Policy:         policy,
		Effective:      settings,
		Outcome:        res.Outcome.Recorded(),
		Containers:     simulationContainers(res, a.Request.Inputs()),
		Trace:          res.Traces,
		InitContainers: initContainerNamesOf(id),
		Resources:      containerResourcesOf(id),
	}

	tr, err := simulationRange(req, a.Request.Now())
	if err != nil {
		return nil, err
	}
	span := chartSpan(tr, cmp.Or(req.Step, "5m"))
	if history, err := recommender.ComputeRange(ctx, s.Inputs, a.Request, span); err != nil {
		out.HistoryError = err.Error()
	} else {
		out.History = s.simulationHistory(ctx, key, span, history)
	}
	return out, nil
}

// simulationRange is the range a Simulation's history is read over: the
// request's absolute range, or its window back from now, the instant the
// Simulation computes as of, so the history's last instant is the Simulation
// itself.
func simulationRange(req simulateRequest, now time.Time) (promclient.TimeRange, error) {
	if req.FromTs > 0 && req.ToTs > 0 {
		return promclient.TimeRange{Start: time.Unix(req.FromTs, 0), End: time.Unix(req.ToTs, 0)}, nil
	}
	tr, err := promclient.TimeRangeFromWindow(recommender.ResourceWindow(req.Window), now)
	if err != nil {
		return promclient.TimeRange{}, fmt.Errorf("resolve chart window: %w", err)
	}
	return tr, nil
}

// chartSpan is the span a chart of r reads a history at, step as its finest.
func chartSpan(r promclient.TimeRange, step string) recommender.Span {
	// The handler validated the step; one it still cannot read is no hint.
	hint, _ := promclient.ParseStep(step)
	return recommender.SpanOf(r.Start.UTC(), r.End.UTC(), hint)
}

// stepSeconds is span's step in seconds, as a chart reads it.
func stepSeconds(span recommender.Span) int64 {
	return int64(span.Step / time.Second)
}

// simulationHistory renders what the Simulation computed over span next to
// the requests and limits the identity's pods ran with then.
func (s *Server) simulationHistory(ctx context.Context, key promclient.WorkloadIdentity, span recommender.Span, r recommender.RangeResult) *simulationHistory {
	step := strconv.FormatInt(stepSeconds(span), 10) + "s"
	running := s.running(ctx, key, promclient.TimeRange{Start: span.Start, End: span.End}, step)
	out := &simulationHistory{
		StepSeconds: stepSeconds(span),
		Containers:  make(map[string]containerHistory, len(r.Containers)),
		OOMKills:    recommender.OOMKills(r),
	}
	for name, c := range r.Containers {
		out.Containers[name] = containerHistory{
			CPU: resourceHistory{
				Usage:              c.CPU.Usage,
				RecommendedRequest: c.CPU.Request,
				RecommendedLimit:   c.CPU.Limit,
				RunningRequest:     running.cpuRequests[name],
				RunningLimit:       running.cpuLimits[name],
			},
			Memory: resourceHistory{
				Usage:              c.Memory.Usage,
				RecommendedRequest: c.Memory.Request,
				RecommendedLimit:   c.Memory.Limit,
				RunningRequest:     running.memoryRequests[name],
				RunningLimit:       running.memoryLimits[name],
			},
		}
	}
	return out
}

// policySettings reads the settings of the Policy named name; none for an
// empty name, which leaves every setting to its default.
func (s *Server) policySettings(ctx context.Context, name string) (simulationSettings, error) {
	spec, err := s.rightSizing(ctx, name)
	if err != nil {
		return simulationSettings{}, fmt.Errorf("%w: %w", errBasePolicy, err)
	}
	return settingsOf(spec), nil
}

// rightSizing reads the right-sizing spec of the Policy named name; the zero
// spec, every setting left to its default, for an empty name.
func (s *Server) rightSizing(ctx context.Context, name string) (sustainv1alpha1.RightSizingSpec, error) {
	if name == "" {
		return sustainv1alpha1.RightSizingSpec{}, nil
	}
	var policy sustainv1alpha1.Policy
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: name}, &policy); err != nil {
		return sustainv1alpha1.RightSizingSpec{}, fmt.Errorf("reading Policy %q: %w", name, err)
	}
	return policy.Spec.RightSizing, nil
}

// simulationContainers renders the computed recommendations next to the raw
// usage they were derived from.
func simulationContainers(res recommender.Result, inputs *recommender.WorkloadInputs) map[string]simulationContainerResult {
	out := make(map[string]simulationContainerResult, len(res.Recs))
	for name, rec := range res.Recs {
		cr := simulationContainerResult{
			CPUUsageCores:      inputs.Usage(name, corev1.ResourceCPU),
			MemoryUsageBytes:   inputs.Usage(name, corev1.ResourceMemory),
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
