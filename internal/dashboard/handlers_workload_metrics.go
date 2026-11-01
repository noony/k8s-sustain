package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

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
	step, perr := parseStepParam(q, 5*time.Minute)
	if perr != nil {
		writeFieldError(w, http.StatusBadRequest, perr.Msg, perr.Field)
		return
	}

	ctx := r.Context()
	key := promclient.WorkloadIdentity{Namespace: namespace, OwnerKind: kind, OwnerName: name}

	// Only usage failures abort the response; the rest are best-effort.
	var (
		average    = make([]promclient.ContainerTimeSeries, len(chartResources))
		averageErr = make([]error, len(chartResources))
		running    runningSeries
		id         *inventory.Identity
		signal     *usageSignal
		oomKills   []promclient.OOMKill
		signalErr  error
	)
	var wg sync.WaitGroup
	for i, res := range chartResources {
		wg.Go(func() {
			average[i], averageErr[i] = s.PromClient.QueryRangeByLabel(ctx, perContainer("avg", res.usage, key), "container", tr, step)
		})
	}
	wg.Go(func() { running = s.running(ctx, key, tr, step) })
	wg.Go(func() {
		// A failed read is tolerated: resources and init containers come back
		// nil, and the signal line and the kills give way to the reason.
		var err error
		switch id, err = s.identity(ctx, key); {
		case apierrors.IsNotFound(err):
			id = &inventory.Identity{Key: key}
		case err != nil:
			s.Logger.Error(err, "failed to read the workload identity", "namespace", namespace, "kind", kind, "name", name)
			signalErr = fmt.Errorf("reading the workload identity: %w", err)
			return
		}
		signal, oomKills, signalErr = s.readHistory(ctx, id, tr, step)
	})
	wg.Wait()

	payload := map[string]any{
		"resources":      containerResourcesOf(id),
		"running":        running,
		"oomKills":       oomKills,
		"initContainers": initContainerNamesOf(id),
		"signal":         signal,
	}
	for i, res := range chartResources {
		if err := averageErr[i]; err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("%s range query: %v", res.name, err))
			return
		}
		payload[string(res.name)] = average[i]
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

// usageSignalSeries is one container's usage signal series, per resource it
// read a value of.
type usageSignalSeries map[corev1.ResourceName][]promclient.TimeValue

// chartResource is one resource the detail page and a Simulation's history
// chart, with the rules its replica-averaged usage and its running request
// and limit are read from.
type chartResource struct {
	name                  corev1.ResourceName
	usage, request, limit string
}

var chartResources = []chartResource{
	{
		name:    corev1.ResourceCPU,
		usage:   promclient.MetricContainerCPUUsageByWorkloadRate1m,
		request: promclient.MetricContainerCPURequestsByWorkloadCores,
		limit:   promclient.MetricContainerCPULimitsByWorkloadCores,
	},
	{
		name:    corev1.ResourceMemory,
		usage:   promclient.MetricContainerMemoryByWorkloadBytes,
		request: promclient.MetricContainerMemoryRequestsByWorkloadBytes,
		limit:   promclient.MetricContainerMemoryLimitsByWorkloadBytes,
	},
}

// readHistory reads id's history over r under its governing Policy's
// settings, through the inputs port a Simulation's history is read through,
// over the span a Simulation charting r would read: what its usage signals
// read, and its OOM kills. The usage signal line is nil when no Policy
// governs id: a Conflicted or ungoverned identity is read under the
// recommender defaults for its kills alone, as no Policy sizes from its
// usage.
func (s *Server) readHistory(ctx context.Context, id *inventory.Identity, r promclient.TimeRange, step time.Duration) (*usageSignal, []promclient.OOMKill, error) {
	spec, err := s.rightSizing(ctx, id.Policy)
	if err != nil {
		return nil, nil, err
	}
	span := chartSpan(r, step)
	read, err := recommender.ReadRange(ctx, s.Inputs, spec, id, span)
	if err != nil {
		return nil, nil, err
	}
	kills := recommender.OOMKills(read)
	if id.Policy == "" {
		return nil, kills, nil
	}
	resources := recommender.WithDefaults(spec.ResourcesConfigs)
	signal := &usageSignal{
		Policy:                id.Policy,
		StepSeconds:           stepSeconds(span),
		CPU:                   usageSignalSettingsOf(resources.CPU),
		Memory:                usageSignalSettingsOf(resources.Memory),
		ExcludeInitContainers: spec.ExcludeInitContainers,
		Containers:            make(map[string]usageSignalSeries, len(read.Containers)),
	}
	for name, c := range read.Containers {
		series := make(usageSignalSeries, len(c))
		for res, rr := range c {
			series[res] = rr.Usage
		}
		signal.Containers[name] = series
	}
	return signal, kills, nil
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
// with over a range, per resource.
type runningSeries map[corev1.ResourceName]runningResource

// runningResource is one resource's requests and limits, per container.
type runningResource struct {
	Requests promclient.ContainerTimeSeries `json:"requests,omitempty"`
	Limits   promclient.ContainerTimeSeries `json:"limits,omitempty"`
}

// running reads the requests and limits key's containers ran with over r,
// the highest of its pods' at each step. A series that cannot be read is
// left nil.
func (s *Server) running(ctx context.Context, key promclient.WorkloadIdentity, r promclient.TimeRange, step time.Duration) runningSeries {
	read := make([]runningResource, len(chartResources))
	var wg sync.WaitGroup
	for i, res := range chartResources {
		wg.Go(func() {
			read[i].Requests, _ = s.PromClient.QueryRangeByLabel(ctx, perContainer("max", res.request, key), "container", r, step)
		})
		wg.Go(func() {
			read[i].Limits, _ = s.PromClient.QueryRangeByLabel(ctx, perContainer("max", res.limit, key), "container", r, step)
		})
	}
	wg.Wait()
	out := make(runningSeries, len(chartResources))
	for i, res := range chartResources {
		out[res.name] = read[i]
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
