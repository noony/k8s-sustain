package dashboard

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

func (s *Server) handleWorkloadMetrics(w http.ResponseWriter, r *http.Request, namespace, kind, name string) {
	q := r.URL.Query()
	tr, perr := parseTimeRange(q, "168h", time.Now())
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}
	step, perr := parseStepParam(q, "5m")
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	ctx := r.Context()
	key := promclient.WorkloadIdentity{Namespace: namespace, OwnerKind: kind, OwnerName: name}

	// Only cpu/memory usage failures abort the response; the rest are
	// best-effort.
	var (
		cpuSeries, memSeries promclient.ContainerTimeSeries
		running              runningSeries
		oomEvents            []promclient.OOMKill
		cpuErr, memErr       error
		id                   *inventory.Identity
		signal               *usageSignal
		signalErr            error
	)
	var wg sync.WaitGroup
	wg.Go(func() {
		cpuSeries, cpuErr = s.PromClient.QueryRangeByLabel(ctx,
			perContainer("avg", promclient.MetricContainerCPUUsageByWorkloadRate1m, key), "container", tr, step)
	})
	wg.Go(func() {
		memSeries, memErr = s.PromClient.QueryRangeByLabel(ctx,
			perContainer("avg", promclient.MetricContainerMemoryByWorkloadBytes, key), "container", tr, step)
	})
	wg.Go(func() { oomEvents = s.oomKills(ctx, key, tr, step) })
	wg.Go(func() { running = s.running(ctx, key, tr, step) })
	wg.Go(func() {
		// A failed read is tolerated: resources and init containers come back
		// nil, and the signal line gives way to the reason.
		var err error
		switch id, err = s.identity(ctx, key); {
		case apierrors.IsNotFound(err):
		case err != nil:
			s.Logger.Error(err, "failed to read the workload identity", "namespace", namespace, "kind", kind, "name", name)
			signalErr = fmt.Errorf("reading the workload identity: %w", err)
		default:
			signal, signalErr = s.usageSignal(ctx, id, tr, step)
		}
	})
	wg.Wait()

	if cpuErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("cpu range query: %v", cpuErr))
		return
	}
	if memErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("memory range query: %v", memErr))
		return
	}

	payload := map[string]any{
		"cpu":            cpuSeries,
		"memory":         memSeries,
		"resources":      containerResourcesOf(id),
		"cpuRequests":    running.cpuRequests,
		"memoryRequests": running.memoryRequests,
		"cpuLimits":      running.cpuLimits,
		"memoryLimits":   running.memoryLimits,
		"oomEvents":      oomEvents,
		"initContainers": initContainerNamesOf(id),
		"signal":         signal,
	}
	if signalErr != nil {
		payload["signalError"] = signalErr.Error()
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, payload)
}

// usageSignal is what an identity's usage signals read over a chart range,
// under its governing Policy's settings: the value the recommender sizes each
// request from, at each instant, which the detail page charts next to the
// replica average. It is only read, never computed from: the page shows the
// stored Recommendation and nothing else.
type usageSignal struct {
	// Policy names the governing Policy the signals read under.
	Policy string `json:"policy"`
	// StepSeconds is the step between its instants, derived from the range:
	// the step a request asks for is only the finest it gets.
	StepSeconds int64               `json:"stepSeconds"`
	CPU         usageSignalSettings `json:"cpu"`
	Memory      usageSignalSettings `json:"memory"`
	// ExcludeInitContainers is set when the Policy recommends no init
	// container, so none has a series.
	ExcludeInitContainers bool `json:"excludeInitContainers,omitempty"`
	// Containers holds, per container the Policy recommends, each resource's
	// series in cores or bytes.
	Containers map[string]usageSignalSeries `json:"containers"`
}

// usageSignalSettings are what one resource's usage signal read under, every
// default explicit. A kept request has no percentile and no series: no signal
// sizes it.
type usageSignalSettings struct {
	Percentile  int32  `json:"percentile,omitempty"`
	Window      string `json:"window"`
	KeepRequest bool   `json:"keepRequest,omitempty"`
}

type usageSignalSeries struct {
	CPU    []promclient.TimeValue `json:"cpu,omitempty"`
	Memory []promclient.TimeValue `json:"memory,omitempty"`
}

// usageSignal reads what id's usage signals read over r under its governing
// Policy's settings, through the inputs port a Simulation's history is read
// through, over the span a Simulation charting r would read. It is nil, with
// no error, when no Policy governs id: a Conflicted or ungoverned identity has
// no signal line.
func (s *Server) usageSignal(ctx context.Context, id *inventory.Identity, r promclient.TimeRange, step string) (*usageSignal, error) {
	if id.Policy == "" {
		return nil, nil
	}
	var policy sustainv1alpha1.Policy
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: id.Policy}, &policy); err != nil {
		return nil, fmt.Errorf("reading Policy %q: %w", id.Policy, err)
	}
	spec := policy.Spec.RightSizing
	// The handler validated the step; one it still cannot read is no hint.
	hint, _ := promclient.ParseStep(step)
	span := recommender.SpanOf(r.Start.UTC(), r.End.UTC(), hint)
	series, err := recommender.UsageRange(ctx, s.Inputs, spec, id, span)
	if err != nil {
		return nil, err
	}
	resources := recommender.WithDefaults(spec.ResourcesConfigs)
	out := &usageSignal{
		Policy:                id.Policy,
		StepSeconds:           int64(span.Step / time.Second),
		CPU:                   usageSignalSettingsOf(resources.CPU),
		Memory:                usageSignalSettingsOf(resources.Memory),
		ExcludeInitContainers: spec.ExcludeInitContainers,
		Containers:            make(map[string]usageSignalSeries, len(series)),
	}
	for name, c := range series {
		out.Containers[name] = usageSignalSeries{CPU: c.CPU, Memory: c.Memory}
	}
	return out, nil
}

func usageSignalSettingsOf(rc sustainv1alpha1.ResourceConfig) usageSignalSettings {
	out := usageSignalSettings{Window: rc.Window, KeepRequest: rc.Requests.KeepRequest}
	if p := rc.Requests.Percentile; p != nil {
		out.Percentile = *p
	}
	return out
}

// perContainer renders rule for the identity key, aggregated per container by
// agg.
func perContainer(agg, rule string, key promclient.WorkloadIdentity) string {
	return fmt.Sprintf("%s by (container) (%s%s)", agg, rule, promclient.WorkloadSelector(key.Namespace, key.OwnerKind, key.OwnerName))
}

// runningSeries are the requests and limits an identity's containers ran
// with over a range, per container.
type runningSeries struct {
	cpuRequests, memoryRequests, cpuLimits, memoryLimits promclient.ContainerTimeSeries
}

// running reads the requests and limits key's containers ran with over r,
// the highest of its pods' at each step. A series that cannot be read is
// left nil.
func (s *Server) running(ctx context.Context, key promclient.WorkloadIdentity, r promclient.TimeRange, step string) runningSeries {
	var out runningSeries
	var wg sync.WaitGroup
	for rule, into := range map[string]*promclient.ContainerTimeSeries{
		promclient.MetricContainerCPURequestsByWorkloadCores:    &out.cpuRequests,
		promclient.MetricContainerMemoryRequestsByWorkloadBytes: &out.memoryRequests,
		promclient.MetricContainerCPULimitsByWorkloadCores:      &out.cpuLimits,
		promclient.MetricContainerMemoryLimitsByWorkloadBytes:   &out.memoryLimits,
	} {
		wg.Go(func() {
			*into, _ = s.PromClient.QueryRangeByLabel(ctx, perContainer("max", rule, key), "container", r, step)
		})
	}
	wg.Wait()
	return out
}

// oomKills reads key's OOM kills over r, each dated at the end of the step it
// fell in, none when they cannot be read: kube-state-metrics may be missing,
// and the page renders without them.
func (s *Server) oomKills(ctx context.Context, key promclient.WorkloadIdentity, r promclient.TimeRange, step string) []promclient.OOMKill {
	stepDur, err := promclient.ParseStep(step)
	if err != nil {
		return nil
	}
	selector := promclient.WorkloadSelector(key.Namespace, key.OwnerKind, key.OwnerName)
	perStep, err := s.PromClient.QueryRangeByLabel(ctx, promclient.OOMKillsPerStep(selector, stepDur), "container", r, step)
	if err != nil {
		return nil
	}
	var out []promclient.OOMKill
	for _, container := range slices.Sorted(maps.Keys(perStep)) {
		out = append(out, promclient.OOMEventsOf(container, perStep[container])...)
	}
	return out
}

type containerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// containerResourcesOf returns the requests and limits each of the identity's
// containers runs with, nil when the identity is unknown.
func containerResourcesOf(id *inventory.Identity) map[string]containerResources {
	if id == nil {
		return nil
	}
	all := append(slices.Clone(id.Containers), id.InitContainers...)
	if len(all) == 0 {
		// Keep the nil map so the JSON stays null rather than {}.
		return nil
	}
	result := make(map[string]containerResources, len(all))
	for _, c := range all {
		cpuReq, cpuLim, memReq, memLim := resourceStrings(c)
		result[c.Name] = containerResources{
			CPURequest:    cpuReq,
			CPULimit:      cpuLim,
			MemoryRequest: memReq,
			MemoryLimit:   memLim,
		}
	}
	return result
}

func initContainerNamesOf(id *inventory.Identity) []string {
	if id == nil || len(id.InitContainers) == 0 {
		return nil
	}
	initCs := id.InitContainers
	out := make([]string, len(initCs))
	for i, c := range initCs {
		out[i] = c.Name
	}
	return out
}
