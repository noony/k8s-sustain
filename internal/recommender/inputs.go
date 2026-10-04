package recommender

import (
	"context"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// WorkloadInputs is one identity's recommendation inputs: the values of every
// signal, by container. CPUPerPod and MemPerPod are already per-pod
// percentiles of the busiest replica — no replica division is applied
// downstream.
type WorkloadInputs struct {
	CPUPerPod promclient.ContainerValues
	MemPerPod promclient.ContainerValues
	// OOM is each container's OOM kills over the past 24h. Empty when it could
	// not be read: recommendations never block on missing OOM data.
	OOM map[string]OOM
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
