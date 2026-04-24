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
	// +optional
	RequestsLimitsRatio *float64 `json:"requestsLimitsRatio,omitempty"`
}

// ResourceConfig holds the recommendation configuration for one resource dimension (CPU or memory).
type ResourceConfig struct {
	// Window is the historical observation window used for recommendation (e.g. "96h").
	// +optional
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

// HpaMode defines how the controller interacts with Horizontal Pod Autoscalers.
// +kubebuilder:validation:Enum=HpaAware;UpdateTargetValue;Ignore
type HpaMode string

const (
	HpaModeHpaAware          HpaMode = "HpaAware"
	HpaModeUpdateTargetValue HpaMode = "UpdateTargetValue"
	HpaModeIgnore            HpaMode = "Ignore"
)

// HpaResourceConfig allows overriding the auto-detected HPA target utilization
// for a specific resource dimension.
type HpaResourceConfig struct {
	// TargetUtilizationOverride overrides the auto-detected HPA target utilization
	// for this resource. When set, the controller uses this value instead of reading
	// the HPA spec. Value is a percentage (1-100).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetUtilizationOverride *int32 `json:"targetUtilizationOverride,omitempty"`
}

// HpaConfig configures how the controller interacts with HPAs targeting
// the same workloads.
type HpaConfig struct {
	// Mode determines the HPA interaction strategy.
	// Default: HpaAware.
	// +optional
	// +kubebuilder:default=HpaAware
	Mode HpaMode `json:"mode,omitempty"`
	// CPU holds optional overrides for CPU-related HPA settings.
	// +optional
	CPU *HpaResourceConfig `json:"cpu,omitempty"`
	// Memory holds optional overrides for memory-related HPA settings.
	// +optional
	Memory *HpaResourceConfig `json:"memory,omitempty"`
}

// RightSizingUpdatePolicy controls eviction-related behaviour during right-sizing.
type RightSizingUpdatePolicy struct {
	// IgnoreAutoscalerSafeToEvictAnnotations skips the cluster-autoscaler safe-to-evict
	// annotation check when restarting pods for right-sizing.
	// +optional
	IgnoreAutoscalerSafeToEvictAnnotations bool `json:"ignoreAutoscalerSafeToEvictAnnotations,omitempty"`
	// Hpa configures interaction with Horizontal Pod Autoscalers.
	// When nil, defaults to HpaAware mode with no overrides (auto-detect and adjust).
	// +optional
	Hpa *HpaConfig `json:"hpa,omitempty"`
}

// RightSizingSpec defines how resource recommendations are computed and applied.
type RightSizingSpec struct {
	// UpdatePolicy controls eviction behaviour.
	// +optional
	UpdatePolicy RightSizingUpdatePolicy `json:"updatePolicy,omitempty"`
	// ResourcesConfigs holds per-resource-dimension recommendation configs.
	// +optional
	ResourcesConfigs ResourcesConfigs `json:"resourcesConfigs,omitempty"`
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

// UpdateSpec defines which workload types are managed and how.
type UpdateSpec struct {
	// Types lists the workload types and their update modes.
	// +optional
	Types UpdateTypes `json:"types,omitempty"`
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
	// Update configures which workload types are reconciled and how.
	// +optional
	Update UpdateSpec `json:"update,omitempty"`
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
