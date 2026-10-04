package recommender

import (
	"context"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// WorkloadInputs bundles the Prometheus query results needed to build
// per-container recommendations for one identity. CPUPerPod and MemPerPod are
// already per-pod percentiles of the busiest replica — no replica division is
// applied downstream.
type WorkloadInputs struct {
	CPUPerPod promclient.ContainerValues
	MemPerPod promclient.ContainerValues
	// OOM is the identity's OOM signal from the past 24h, with per-container
	// OOM counts. Empty when it could not be read: recommendations never block
	// on missing OOM data.
	OOM promclient.OOMSignal
}

// HasRecentOOM reports recent OOM activity in any container, for deciding
// whether to bypass the workload-age gate. Per-container recency, which drives
// the memory floor, comes from OOM.OOMCounts instead.
func (w *WorkloadInputs) HasRecentOOM() bool {
	return w.OOM.TotalOOMs() > 0
}

// ObservedContainers lists every container Prometheus reported on, sorted:
// usage series plus the containers that OOMed, so a crash-looping container
// with no usage samples still gets a memory recommendation.
func (w *WorkloadInputs) ObservedContainers() []corev1.Container {
	names := make(map[string]struct{}, len(w.CPUPerPod)+len(w.MemPerPod))
	for n := range w.CPUPerPod {
		names[n] = struct{}{}
	}
	for n := range w.MemPerPod {
		names[n] = struct{}{}
	}
	for n, count := range w.OOM.OOMCounts {
		if count > 0 {
			names[n] = struct{}{}
		}
	}
	out := make([]corev1.Container, 0, len(names))
	for _, n := range slices.Sorted(maps.Keys(names)) {
		out = append(out, corev1.Container{Name: n})
	}
	return out
}

// InputsRequest asks for one identity's recommendation inputs.
type InputsRequest struct {
	Identity promclient.WorkloadIdentity
	// Containers is the identity's container count, a size hint for batching.
	// Zero means unknown.
	Containers int
}

// InputsResult is one identity's inputs, or the error that made them
// unavailable. Inputs is non-nil exactly when Err is nil.
type InputsResult struct {
	Inputs *WorkloadInputs
	Err    error
}

// InputsFetcher fetches the recommendation inputs of many identities at once,
// all under one ResourcesConfigs. The result holds exactly one entry per
// requested identity. An identity with no samples gets empty Inputs, not an
// error: an error means its inputs could not be read at all.
type InputsFetcher interface {
	FetchInputs(ctx context.Context, cfg sustainv1alpha1.ResourcesConfigs, reqs []InputsRequest) map[promclient.WorkloadIdentity]InputsResult
}
