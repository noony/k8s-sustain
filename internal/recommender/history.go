package recommender

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// maxHistoryPoints bounds the instants a history is read at, so a chart of it
// has at most about as many points whatever its range.
const maxHistoryPoints = 300

// Span is the instants a history is read at: End, and every Step before it
// back to Start.
type Span struct {
	Start, End time.Time
	Step       time.Duration
}

// SpanOf is the span of [start, end] at the finest step, never finer than
// hint, that reads it at no more than maxHistoryPoints instants. The step is
// a whole multiple of hint, or of a second without one. The span ends at end,
// so its last instant is the one a computation as of end reads.
func SpanOf(start, end time.Time, hint time.Duration) Span {
	unit := max(hint, time.Second)
	units := int64(1)
	if d := end.Sub(start); d > 0 {
		finest := (d + maxHistoryPoints - 2) / (maxHistoryPoints - 1)
		units = max(int64((finest+unit-1)/unit), 1)
	}
	step := time.Duration(units) * unit
	steps := max(int64(end.Sub(start)/step), 0)
	return Span{Start: end.Add(-time.Duration(steps) * step), End: end, Step: step}
}

// instants is how many instants s reads.
func (s Span) instants() int {
	return int(s.End.Sub(s.Start)/s.Step) + 1
}

// instant is the i-th instant of s, the earliest first.
func (s Span) instant(i int) time.Time {
	return s.Start.Add(time.Duration(i) * s.Step)
}

// index is the instant of s nearest t, false when t is outside s.
func (s Span) index(t time.Time) (int, bool) {
	i := int((t.Sub(s.Start) + s.Step/2) / s.Step)
	return i, t.Compare(s.Start.Add(-s.Step/2)) >= 0 && i < s.instants()
}

func (s Span) timeRange() promclient.TimeRange {
	return promclient.TimeRange{Start: s.Start, End: s.End}
}

// History is one identity's recommendation inputs at each instant of a span,
// the earliest first. It never changes once built.
type History struct {
	points []HistoryPoint
}

// HistoryPoint is one identity's inputs as of At.
type HistoryPoint struct {
	At     time.Time
	Inputs *WorkloadInputs
}

// NewHistory builds a history of points, the earliest first. It is for an
// InputsFetcher serving canned inputs, such as recommendertest.StaticInputs;
// build each point's inputs with NewInputs.
func NewHistory(points []HistoryPoint) *History {
	return &History{points: slices.Clone(points)}
}

// HistoryResult is one identity's history, or the error that made it
// unavailable. History is non-nil exactly when Err is nil.
type HistoryResult struct {
	History *History
	Err     error
}

// RangeResult is one identity's series over a span, per container and
// resource: what its signals read at each instant and, from ComputeRange,
// what the chain computed there. It keeps the history it was read from,
// which a signal's own accessor reads (OOMKills).
type RangeResult struct {
	Containers map[string]ContainerRange
	history    *History
}

// ContainerRange is one container's resources at each instant of a span,
// keyed by resource.
type ContainerRange map[corev1.ResourceName]ResourceRange

// ResourceRange is one resource of one container at each instant of a span,
// in cores or bytes: the usage its signal read, and the request and limit the
// chain computed. A series has a point only at the instants it has a value:
// the request none where no signal justified one or the Policy keeps it, the
// limit none where the container would run without one.
type ResourceRange struct {
	Usage, Request, Limit []promclient.TimeValue
}

// ComputeRange is what req computes at each instant of span: the full chain,
// request side and limit side, on every signal's values as of the instant,
// with the instant in its stages' context, so the OOM floor at an instant is
// the floor as it stood then. What is not a signal is today's: the
// autoscaler req was assembled with coordinates every instant, as an
// autoscaler has no history, and the containers' own requests and limits are
// those the identity holds now. The history is read through inputs in one range read,
// under req's resources; the chain then runs at each instant without reading
// anything else. The error is the one that made the history unavailable.
func ComputeRange(ctx context.Context, inputs InputsFetcher, req Request, span Span) (RangeResult, error) {
	h := inputs.FetchHistory(ctx, span, req.resources, []InputsRequest{req.fetch})[req.fetch.Identity]
	if h.Err != nil {
		return RangeResult{}, h.Err
	}
	return computeRange(req, h.History), nil
}

// ReadRange is what id's signals read at each instant of span, under spec's
// resources with every default explicit, as Assemble fetches them, and
// computes nothing from it: each resource's Usage, the busiest replica's
// usage percentile over the resource's window, of every container Assemble
// would size, and no Request or Limit; the kills come through OOMKills. A
// resource whose request spec keeps has no series, as no signal sizes it.
// The history is read through inputs in the one range read ComputeRange
// makes: it charts what the recommender reads without recomputing a
// Recommendation.
func ReadRange(ctx context.Context, inputs InputsFetcher, spec sustainv1alpha1.RightSizingSpec, id *inventory.Identity, span Span) (RangeResult, error) {
	resources := WithDefaults(spec.ResourcesConfigs)
	req := InputsRequest{Identity: id.Key, Containers: len(ContainersOf(spec, id))}
	h := inputs.FetchHistory(ctx, span, resources, []InputsRequest{req})[id.Key]
	if h.Err != nil {
		return RangeResult{}, h.Err
	}
	out := RangeResult{Containers: map[string]ContainerRange{}, history: h.History}
	for _, p := range h.History.points {
		for _, c := range sizedContainers(spec, id, p.Inputs) {
			for _, res := range resourceKinds {
				v, ok := p.Inputs.usage(c.Name, res)
				if !ok || res.config(resources).Requests.KeepRequest {
					continue
				}
				out.update(c.Name, res, func(rr *ResourceRange) {
					rr.Usage = append(rr.Usage, promclient.TimeValue{Timestamp: p.At, Value: v})
				})
			}
		}
	}
	return out, nil
}

func computeRange(req Request, h *History) RangeResult {
	out := RangeResult{Containers: make(map[string]ContainerRange, len(req.containers)), history: h}
	if h == nil {
		return out
	}
	for _, p := range h.points {
		at := req
		at.inputs, at.now = p.Inputs, p.At
		for _, c := range req.containers {
			for _, res := range resourceKinds {
				out.update(c.Name, res, func(rr *ResourceRange) { rr.add(res, c, at) })
			}
		}
	}
	return out
}

// update applies f to res's series of the container named name, creating
// them empty.
func (r RangeResult) update(name string, res resourceKind, f func(*ResourceRange)) {
	c, ok := r.Containers[name]
	if !ok {
		c = make(ContainerRange, len(resourceKinds))
		r.Containers[name] = c
	}
	rr := c[corev1.ResourceName(res)]
	f(&rr)
	c[corev1.ResourceName(res)] = rr
}

// add appends what req computes for res of c as of req.now.
func (rr *ResourceRange) add(res resourceKind, c corev1.Container, req Request) {
	if v, ok := req.inputs.usage(c.Name, res); ok {
		rr.Usage = append(rr.Usage, promclient.TimeValue{Timestamp: req.now, Value: v})
	}
	sized, _ := size(res, c, req)
	if sized == nil {
		return
	}
	rr.Request = append(rr.Request, promclient.TimeValue{Timestamp: req.now, Value: res.value(sized.request)})
	if l := sized.limit.value; l != nil {
		rr.Limit = append(rr.Limit, promclient.TimeValue{Timestamp: req.now, Value: res.value(*l)})
	}
}
