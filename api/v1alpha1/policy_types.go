package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// PolicyAnnotation is the annotation key set on a workload's pod template
	// (spec.template.metadata.annotations) to declare which Policy governs it.
	// Pods inherit the annotation, so the admission webhook reads the same value.
	//
	// Example: k8s.sustain.io/policy: my-rightsizing-policy
	PolicyAnnotation = "k8s.sustain.io/policy"
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
	// Percentile is the histogram percentile used for the recommendation (e.g. 95).
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
	// RequestsLimitsRatio explicitly sets the limit as a multiple of the request.
	// Must be >= 1 so the derived limit is never below the request.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RequestsLimitsRatio *float64 `json:"requestsLimitsRatio,omitempty"`
}

// ResourceConfig holds the recommendation configuration for one resource dimension (CPU or memory).
type ResourceConfig struct {
	// Window is the historical observation window used for recommendation (e.g. "96h").
	// Must be a Prometheus duration: integer followed by one of ms, s, m, h, d, w, y
	// (compounds like "1h30m" are also allowed).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(ms|s|m|h|d|w|y))+$`
	Window string `json:"window,omitempty"`
	// Requests configures how resource requests are computed.
	// +optional
	Requests ResourceRequestsConfig `json:"requests,omitempty"`
	// Limits configures how resource limits are derived from requests.
	// +optional
	Limits ResourceLimitsConfig `json:"limits,omitempty"`
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
	// IgnoreAutoscalerSafeToEvictAnnotations skips the cluster-autoscaler safe-to-evict
	// annotation check when restarting pods for right-sizing.
	// +optional
	IgnoreAutoscalerSafeToEvictAnnotations bool `json:"ignoreAutoscalerSafeToEvictAnnotations,omitempty"`
}

// AutoscalerCoordination configures HPA/ScaledObject-aware request shaping.
type AutoscalerCoordination struct {
	// Enabled turns on the overhead formula for any resource the autoscaler
	// targets on averageUtilization (HPA Resource metric or KEDA cpu/memory trigger).
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ReplicaBudgetAnchor enables CPU replica-budget correction. Value is the
	// fraction into [minReplicas, maxReplicas] at which the workload should sit
	// at steady state. Typical value: 0.10. Nil disables replica correction.
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
	// CronJob patches the job template so future runs use updated resources.
	// OnCreate injects resources at pod admission for each spawned job pod.
	CronJob *UpdateMode `json:"cronJob,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	Job *UpdateMode `json:"job,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	Family *UpdateMode `json:"family,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	DeploymentConfig *UpdateMode `json:"deploymentConfig,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	ArgoRollout *UpdateMode `json:"argoRollout,omitempty"`
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
	// Namespaces is a list of namespaces to target.
	// An empty list targets all namespaces.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// LabelSelector restricts the set of workloads targeted by this policy.
	// An empty selector matches all workloads in the targeted namespaces.
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

func init() {
	SchemeBuilder.Register(&Policy{}, &PolicyList{})
}
