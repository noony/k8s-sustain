package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// PolicyAnnotation names the Policy governing a workload. Pods inherit
	// it from the pod template, so the webhook reads the same value.
	//
	// Example: k8s.sustain.io/policy: my-rightsizing-policy
	PolicyAnnotation = "k8s.sustain.io/policy"

	// OwnerNameAnnotation overrides the workload identity derived from the
	// ownerReferences chain: a bare pod becomes kind "Pod" with this name, a
	// pod with a real owner is grouped under this shared identity. The webhook
	// mirrors it to a pod label, so the value must be a valid label value
	// (RFC 1123, at most 63 chars).
	//
	// Example: k8s.sustain.io/owner-name: etl-daily
	OwnerNameAnnotation = "k8s.sustain.io/owner-name"

	// OptOutAnnotation excludes a workload from a Policy inherited from a less
	// specific level (Namespace, or metadata when the pod template is
	// evaluated). Only the literal string "true" opts out.
	//
	// Example: k8s.sustain.io/opt-out: "true"
	OptOutAnnotation = "k8s.sustain.io/opt-out"

	// WLRPolicyLabel labels each WorkloadRecommendation with the Policy that
	// produced it, so list calls can be scoped server-side.
	WLRPolicyLabel = "k8s.sustain.io/policy"

	// WLRStubLabel marks a WorkloadRecommendation created by the webhook at
	// admission rather than by the controller. Provenance only; nothing reads
	// it to decide behavior.
	WLRStubLabel = "k8s.sustain.io/stub"
)

// UpdateMode defines how resources are updated on a given workload type.
// +kubebuilder:validation:Enum=OnCreate;Ongoing
type UpdateMode string

const (
	UpdateModeOnCreate UpdateMode = "OnCreate"
	UpdateModeOngoing  UpdateMode = "Ongoing"
)

// ResourceRequestsConfig configures how resource requests are computed.
// +kubebuilder:validation:XValidation:rule="!has(self.minAllowed) || !has(self.maxAllowed) || quantity(self.minAllowed).compareTo(quantity(self.maxAllowed)) <= 0",message="minAllowed must be less than or equal to maxAllowed"
// +kubebuilder:validation:XValidation:rule="!(has(self.keepRequest) && self.keepRequest) || (!has(self.headroom) && !has(self.percentile) && !has(self.minAllowed) && !has(self.maxAllowed))",message="keepRequest cannot be combined with headroom, percentile, minAllowed, or maxAllowed (they have no effect when the request is kept)"
type ResourceRequestsConfig struct {
	// Headroom adds a safety buffer on top of the computed recommendation (percentage, 0-100).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Headroom *int32 `json:"headroom,omitempty"`
	// KeepRequest disables request updates when true.
	// +optional
	KeepRequest bool `json:"keepRequest,omitempty"`
	// MaxAllowed caps the recommended request value.
	// +optional
	MaxAllowed *resource.Quantity `json:"maxAllowed,omitempty"`
	// MinAllowed floors the recommended request value.
	// +optional
	MinAllowed *resource.Quantity `json:"minAllowed,omitempty"`
	// Percentile is the histogram percentile used for the recommendation (e.g. 95); 100 resolves to the maximum sample over the window.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percentile *int32 `json:"percentile,omitempty"`
}

// ResourceLimitsConfig configures how resource limits are set relative to requests.
// +kubebuilder:validation:XValidation:rule="((has(self.equalsToRequest) && self.equalsToRequest) ? 1 : 0) + ((has(self.keepLimit) && self.keepLimit) ? 1 : 0) + ((has(self.keepLimitRequestRatio) && self.keepLimitRequestRatio) ? 1 : 0) + ((has(self.noLimit) && self.noLimit) ? 1 : 0) + (has(self.requestsLimitsRatio) ? 1 : 0) <= 1",message="at most one of equalsToRequest, keepLimit, keepLimitRequestRatio, noLimit, requestsLimitsRatio may be set"
type ResourceLimitsConfig struct {
	// EqualsToRequest sets the limit equal to the computed request.
	// +optional
	EqualsToRequest bool `json:"equalsToRequest,omitempty"`
	// KeepLimit leaves the existing limit unchanged.
	// +optional
	KeepLimit bool `json:"keepLimit,omitempty"`
	// KeepLimitRequestRatio preserves the current limit-to-request ratio.
	// +optional
	KeepLimitRequestRatio bool `json:"keepLimitRequestRatio,omitempty"`
	// NoLimit removes the limit entirely.
	// +optional
	NoLimit bool `json:"noLimit,omitempty"`
	// RequestsLimitsRatio sets the limit as a multiple (>= 1) of the request.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RequestsLimitsRatio *float64 `json:"requestsLimitsRatio,omitempty"`
}

// ResourceConfig holds the recommendation configuration for one resource dimension (CPU or memory).
type ResourceConfig struct {
	// Window is the Prometheus duration of the observation window used for recommendation (e.g. "96h").
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(m|h|d|w|y))+$`
	Window string `json:"window,omitempty"`
	// Requests configures how resource requests are computed.
	// +optional
	Requests ResourceRequestsConfig `json:"requests,omitempty"`
	// Limits configures how resource limits are derived from requests.
	// +optional
	Limits ResourceLimitsConfig `json:"limits,omitempty"`
	// DownsizeThreshold suppresses decreases smaller than max(percent% of current, minDecrease); defaults to 5% and 10m CPU / 15Mi memory when unset.
	// +optional
	DownsizeThreshold *DownsizeThreshold `json:"downsizeThreshold,omitempty"`
}

// DownsizeThreshold gates whether a resource decrease is large enough to
// justify recycling a pod. Increases are never gated.
type DownsizeThreshold struct {
	// Percent is the minimum decrease as a percentage of the current value (default 5).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent *int32 `json:"percent,omitempty"`
	// MinDecrease is the minimum decrease as an absolute quantity (default 10m for CPU, 15Mi for memory).
	// +optional
	MinDecrease *resource.Quantity `json:"minDecrease,omitempty"`
}

// ResourcesConfigs groups CPU and memory recommendation configs.
type ResourcesConfigs struct {
	// CPU holds the recommendation config for CPU resources.
	// +optional
	CPU ResourceConfig `json:"cpu,omitempty"`
	// Memory holds the recommendation config for memory resources.
	// +optional
	Memory ResourceConfig `json:"memory,omitempty"`
}

// EvictionPolicy controls eviction-related behaviour during right-sizing.
type EvictionPolicy struct {
	// IgnoreAutoscalerSafeToEvictAnnotations evicts pods even when they carry cluster-autoscaler.kubernetes.io/safe-to-evict: "false".
	// +optional
	IgnoreAutoscalerSafeToEvictAnnotations bool `json:"ignoreAutoscalerSafeToEvictAnnotations,omitempty"`
}

// AutoscalerCoordination configures HPA/ScaledObject-aware request shaping.
type AutoscalerCoordination struct {
	// Enabled turns on the overhead formula for any resource the autoscaler targets on averageUtilization.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ReplicaBudgetAnchor is the fraction into [minReplicas, maxReplicas] the workload should sit at steady state (typically 0.10); nil disables replica correction.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	ReplicaBudgetAnchor *float64 `json:"replicaBudgetAnchor,omitempty"`
}

// RightSizingSpec defines how resource recommendations are computed and applied.
type RightSizingSpec struct {
	// Update configures which workload types are reconciled and how.
	// +optional
	Update UpdateSpec `json:"update,omitempty"`
	// ResourcesConfigs holds per-resource-dimension recommendation configs.
	// +optional
	ResourcesConfigs ResourcesConfigs `json:"resourcesConfigs,omitempty"`
	// AutoscalerCoordination configures HPA/ScaledObject-aware request shaping.
	// +optional
	AutoscalerCoordination AutoscalerCoordination `json:"autoscalerCoordination,omitempty"`
	// ExcludeInitContainers skips init containers, including restartable sidecars, for every workload this policy targets.
	// +optional
	ExcludeInitContainers bool `json:"excludeInitContainers,omitempty"`
	// RecommendOnly puts every workload governed by this policy in dry-run; it is ORed with the global --recommend-only flag.
	// +optional
	RecommendOnly bool `json:"recommendOnly,omitempty"`
}

// UpdateTypes defines the update mode for each supported workload kind.
type UpdateTypes struct {
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	Deployment *UpdateMode `json:"deployment,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	StatefulSet *UpdateMode `json:"statefulSet,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	DaemonSet *UpdateMode `json:"daemonSet,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	// CronJob never mutates the CronJob spec: OnCreate injects resources at pod admission, Ongoing additionally resizes running job pods in place.
	CronJob *UpdateMode `json:"cronJob,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	Job *UpdateMode `json:"job,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	ArgoRollout *UpdateMode `json:"argoRollout,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	// Pod targets bare pods that opt in via the owner-name annotation: OnCreate injects resources at admission, Ongoing additionally resizes running pods in place (k8s >= 1.33); bare pods are never evicted.
	Pod *UpdateMode `json:"pod,omitempty"`
}

// ModeForKind returns the update mode configured for the given workload
// kind, or nil if the policy does not opt that kind in.
func (t UpdateTypes) ModeForKind(kind string) *UpdateMode {
	switch kind {
	case "Deployment":
		return t.Deployment
	case "StatefulSet":
		return t.StatefulSet
	case "DaemonSet":
		return t.DaemonSet
	case "CronJob":
		return t.CronJob
	case "Job":
		return t.Job
	case "Rollout":
		return t.ArgoRollout
	case "Pod":
		return t.Pod
	}
	return nil
}

// EffectiveRecommendOnly reports whether recommendations for this policy
// must not be applied: the global flag is a master switch ORed with the
// per-policy field. Both injection paths must gate on this.
func (p *Policy) EffectiveRecommendOnly(global bool) bool {
	return global || p.Spec.RightSizing.RecommendOnly
}

// UpdateSpec defines which workload types are managed and how, plus eviction behaviour.
type UpdateSpec struct {
	// Types lists the workload types and their update modes.
	// +optional
	Types UpdateTypes `json:"types,omitempty"`
	// Eviction controls eviction behaviour during right-sizing.
	// +optional
	Eviction EvictionPolicy `json:"eviction,omitempty"`
}

// PolicySelector defines which namespaces and workloads a Policy applies to.
type PolicySelector struct {
	// Namespaces is a list of namespaces to target; an empty list targets all namespaces.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// LabelSelector restricts the set of workloads targeted by this policy; an empty selector matches all.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// PolicySpec defines the desired state of a Policy.
type PolicySpec struct {
	// Selector defines which namespaces and workloads this policy applies to.
	// +optional
	Selector PolicySelector `json:"selector,omitempty"`
	// RightSizing configures resource recommendation and application.
	// +optional
	RightSizing RightSizingSpec `json:"rightSizing,omitempty"`
}

// PolicyStatus defines the observed state of a Policy.
type PolicyStatus struct {
	// Conditions represent the latest available observations of the Policy state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Policy is the Schema for the policies API.
type Policy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PolicySpec   `json:"spec,omitempty"`
	Status PolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PolicyList contains a list of Policy.
type PolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Policy `json:"items"`
}
