package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRecommendationSpec identifies the workload this recommendation
// applies to, and the Policy that produced it.
type WorkloadRecommendationSpec struct {
	// WorkloadRef identifies the workload these recommendations describe.
	WorkloadRef WorkloadReference `json:"workloadRef"`

	// Policy is the Policy that last governed the identity, which records its decisions here. A Conflicted identity keeps it, and the webhook injects only into pods that opt into this Policy.
	// +optional
	Policy string `json:"policy,omitempty"`
}

// WorkloadReference uniquely identifies a workload within the cluster. For
// kind Pod, Name is the owner-name annotation value, not an object name.
type WorkloadReference struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;CronJob;Job;Rollout;Pod
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// RecommendationOutcome is what the controller last decided for an identity.
// +kubebuilder:validation:Enum=Computed;NoData;TooYoung;FetchFailed;Conflicted
type RecommendationOutcome string

const (
	// OutcomeComputed: containers holds a Recommendation computed this pass.
	OutcomeComputed RecommendationOutcome = "Computed"

	// OutcomeNoData: Prometheus answered with nothing recommendable. Not
	// terminal: the identity is recomputed every reconcile cycle.
	OutcomeNoData RecommendationOutcome = "NoData"

	// OutcomeTooYoung: the identity is younger than the minimum age.
	OutcomeTooYoung RecommendationOutcome = "TooYoung"

	// OutcomeFetchFailed: the identity's inputs could not be read.
	OutcomeFetchFailed RecommendationOutcome = "FetchFailed"

	// OutcomeConflicted: members opt into different Policies, so no Policy
	// governs the identity and its Recommendation is frozen.
	OutcomeConflicted RecommendationOutcome = "Conflicted"
)

// WorkloadRecommendationStatus is the observed recommendation, written by the
// controller and read by the webhook as its only recommendation source.
type WorkloadRecommendationStatus struct {
	// ObservedAt is when containers was last computed from Prometheus.
	// +optional
	ObservedAt metav1.Time `json:"observedAt,omitempty"`

	// Outcome is what the controller's last pass decided for the identity: Computed, NoData, TooYoung, FetchFailed or Conflicted. Every outcome but Computed keeps the containers of the last Recommendation.
	// +optional
	Outcome RecommendationOutcome `json:"outcome,omitempty"`

	// ComputedBy is the Policy that computed containers. The webhook injects them only into pods of that Policy, so after another Policy adopts the identity they are withheld until it computes its own.
	// +optional
	ComputedBy string `json:"computedBy,omitempty"`

	// Departed marks a recommendation retained for a workload identity that no longer exists (a TTL-deleted Job, a bare-pod group between runs), whose ObservedAt is therefore frozen.
	// +optional
	Departed bool `json:"departed,omitempty"`

	// ServableUntil is when the webhook stops injecting containers, stamped by the controller with every status write: the staleness budget (the larger of 30m and three reconcile intervals) past ObservedAt for a live identity, the retention window past ObservedAt for a Departed or Conflicted one. Unset when there is no Recommendation; a Recommendation without it is not injected.
	// +optional
	ServableUntil metav1.Time `json:"servableUntil,omitempty"`

	// Containers maps container name to recommended resources.
	// +optional
	Containers map[string]ContainerRecommendation `json:"containers,omitempty"`

	// Trace maps container name to how its recommended resources were derived, stage by stage, request then limit. Written with containers; a trace-only change does not cause a write.
	// +optional
	Trace map[string]ContainerTrace `json:"trace,omitempty"`

	// ObservedResources maps container name to the requests and limits the container actually ran with when the recommendation was written.
	// +optional
	ObservedResources map[string]ObservedContainerResources `json:"observedResources,omitempty"`
}

// ContainerRecommendation is the per-container recommended resource set. An
// unset quantity means "leave the spec entry alone"; RemoveCPULimit and
// RemoveMemoryLimit carry the explicit "strip the limit" intent.
type ContainerRecommendation struct {
	// +optional
	CPURequest *resource.Quantity `json:"cpuRequest,omitempty"`
	// +optional
	MemoryRequest *resource.Quantity `json:"memoryRequest,omitempty"`
	// +optional
	CPULimit *resource.Quantity `json:"cpuLimit,omitempty"`
	// +optional
	MemoryLimit *resource.Quantity `json:"memoryLimit,omitempty"`
	// +optional
	RemoveCPULimit bool `json:"removeCpuLimit,omitempty"`
	// +optional
	RemoveMemoryLimit bool `json:"removeMemoryLimit,omitempty"`
}

// ContainerTrace is the record of how one container's recommendation was
// derived. A resource whose request is kept, or that had nothing to recommend
// from, has none.
type ContainerTrace struct {
	// +optional
	CPU *ResourceTrace `json:"cpu,omitempty"`
	// +optional
	Memory *ResourceTrace `json:"memory,omitempty"`
}

// ResourceTrace is one resource's computation, stage by stage, in the order
// the stages ran: the request's, then the limit's.
type ResourceTrace struct {
	// Request lists the stages that shaped the request: each signal's contribution, then headroom, the min/max clamp and autoscaler coordination. The last stage holds the request.
	// +optional
	Request []TraceStage `json:"request,omitempty"`

	// Limit lists the stages that shaped the limit: the Policy's limit strategy, each signal that may raise the limit, then the guard that raises a limit below the request to the request. Unlike the request's, the last stage need not hold the limit: a signal's stage holds what it contributed even when that did not raise the limit. The Recommendation holds the limit.
	// +optional
	Limit []TraceStage `json:"limit,omitempty"`
}

// TraceStage is one stage of a resource's computation. A stage that did not
// run is absent.
type TraceStage struct {
	// Stage names the stage: percentile, oomFloor, headroom, clamp, coordination, the limit strategy (keepLimit, noLimit, equalsToRequest, requestsLimitsRatio, keepLimitRequestRatio), guard, or another signal's name.
	Stage string `json:"stage"`

	// Value is what a signal contributed, before headroom or rounding; for any other stage, the request or the limit after it. Unset on a limit stage that leaves the container without a limit.
	// +optional
	Value *resource.Quantity `json:"value,omitempty"`

	// Removed is true on a limit stage that strips the limit.
	// +optional
	Removed bool `json:"removed,omitempty"`

	// Facts are the stage's own details as key/values, such as coordination's factors or the limit an OOM kill happened at.
	// +optional
	Facts map[string]string `json:"facts,omitempty"`
}

// ObservedContainerResources is the requests/limits snapshot of one container
// at the last observation. Nil means the container had no value set.
type ObservedContainerResources struct {
	// Init marks containers that come from the pod's initContainers list.
	// +optional
	Init bool `json:"init,omitempty"`
	// +optional
	CPURequest *resource.Quantity `json:"cpuRequest,omitempty"`
	// +optional
	MemoryRequest *resource.Quantity `json:"memoryRequest,omitempty"`
	// +optional
	CPULimit *resource.Quantity `json:"cpuLimit,omitempty"`
	// +optional
	MemoryLimit *resource.Quantity `json:"memoryLimit,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wlrec
// +kubebuilder:printcolumn:name="Workload",type="string",JSONPath=".spec.workloadRef.kind"
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.workloadRef.name"
// +kubebuilder:printcolumn:name="Policy",type="string",JSONPath=".spec.policy"
// +kubebuilder:printcolumn:name="Outcome",type="string",JSONPath=".status.outcome"
// +kubebuilder:printcolumn:name="ObservedAt",type="date",JSONPath=".status.observedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// WorkloadRecommendation is the cached output of the recommendation pipeline
// for a single workload, written by the controller and read by the webhook.
type WorkloadRecommendation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadRecommendationSpec   `json:"spec,omitempty"`
	Status WorkloadRecommendationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadRecommendationList contains a list of WorkloadRecommendation.
type WorkloadRecommendationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadRecommendation `json:"items"`
}
