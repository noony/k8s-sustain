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

	// OwnerNameAnnotation overrides the workload identity (used for Prometheus
	// queries and WorkloadRecommendation naming) that would otherwise be
	// derived from a pod's ownerReferences chain. Two uses: (1) a bare pod
	// with no controller owner (e.g. Airflow's KubernetesPodOperator) sets
	// this to get treated as kind "Pod" with the given name; (2) a pod with a
	// real owner sets this to group multiple workloads (e.g. blue/green
	// Deployments) under one shared identity, while still keeping its real
	// kind. The same string is reused as a Kubernetes LABEL key once the
	// admission webhook mirrors it onto the pod (see internal/webhook) — so
	// the value must validate as a label value (RFC 1123, <=63 chars), not
	// just an annotation value.
	//
	// Example: k8s.sustain.io/owner-name: etl-daily
	OwnerNameAnnotation = "k8s.sustain.io/owner-name"

	// WLRPolicyLabel labels each WorkloadRecommendation with the Policy that
	// produced it, so writers and consumers (controller, webhook, dashboard) can
	// scope list calls server-side instead of post-filtering by spec.policy.
	WLRPolicyLabel = "k8s.sustain.io/policy"
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
	// Percentile is the histogram percentile used for the recommendation (e.g. 95).
	// p100 is allowed and resolves to the maximum sample over the window —
	// useful for memory on workloads where you never want to undershoot peak.
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
	// Must be a Prometheus duration: integer followed by one of m, h, d, w, y
	// (compounds like "1h30m" are also allowed).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(m|h|d|w|y))+$`
	Window string `json:"window,omitempty"`
	// Requests configures how resource requests are computed.
	// +optional
	Requests ResourceRequestsConfig `json:"requests,omitempty"`
	// Limits configures how resource limits are derived from requests.
	// +optional
	Limits ResourceLimitsConfig `json:"limits,omitempty"`
	// DownsizeThreshold suppresses pod recycling for resource DECREASES smaller
	// than max(percent% of current, minDecrease). Increases always apply
	// immediately. Defaults apply when unset (percent 5; minDecrease 10m for
	// CPU, 15Mi for memory). Set both percent and minDecrease to 0 to disable.
	// +optional
	DownsizeThreshold *DownsizeThreshold `json:"downsizeThreshold,omitempty"`
}

// DownsizeThreshold gates whether a resource DECREASE is large enough to justify
// recycling a pod. A decrease is applied only when it meets or exceeds
// max(Percent% of the current value, MinDecrease). Increases are never gated.
type DownsizeThreshold struct {
	// Percent is the minimum decrease as a percentage of the current value.
	// Defaults to 5 when unset.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent *int32 `json:"percent,omitempty"`
	// MinDecrease is the minimum decrease as an absolute quantity. Defaults to
	// 10m (CPU) or 15Mi (memory) when unset.
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
	// IgnoreAutoscalerSafeToEvictAnnotations, when true, evicts pods during
	// right-sizing even if they carry the cluster-autoscaler annotation
	// `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`. By default
	// (false) such pods are never evicted. Only the literal value "false"
	// blocks; in-place resizes are unaffected by the annotation either way.
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
	// ExcludeInitContainers skips init containers (including restartable
	// sidecar init containers) for any workload this policy targets. Defaults
	// to false: init containers are recommended and resized like regular ones.
	// +optional
	ExcludeInitContainers bool `json:"excludeInitContainers,omitempty"`
	// RecommendOnly, when true, puts every workload governed by this policy
	// in dry-run: recommendations are still computed, exported as metrics and
	// cached as WorkloadRecommendations, but the controller never recycles or
	// resizes pods and the webhook never injects resources. ORed with the
	// global --recommend-only flag — the flag is a master switch, so an
	// explicit false here cannot override it.
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
	// CronJob never mutates the CronJob spec (no GitOps drift). OnCreate
	// injects resources at pod admission for each spawned job pod. Ongoing
	// additionally resizes currently-running job pods in place via
	// pods/resize when the cluster supports it; new runs are always handled
	// by the webhook at admission.
	CronJob *UpdateMode `json:"cronJob,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	Job *UpdateMode `json:"job,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	ArgoRollout *UpdateMode `json:"argoRollout,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=OnCreate;Ongoing
	// Pod targets bare pods that opt in via OwnerNameAnnotation (no controller
	// owner — e.g. Airflow's KubernetesPodOperator). OnCreate injects
	// resources at admission like every other kind. Ongoing computes and
	// caches a recommendation the same as any kind, but NEVER recycles the
	// pod: there is no controller that could recreate it after an eviction
	// or in-place resize, so the "Ongoing" half of the contract that applies
	// to every other kind does not apply here.
	Pod *UpdateMode `json:"pod,omitempty"`
}

// ModeForKind returns the update mode configured for the given workload
// kind, or nil if the policy doesn't opt that kind in. The string keys here
// match the values that internal/webhook.resolveOwner, the controller's
// reconcile loop, and the dashboard handlers all use, so callers can plug
// owner_kind labels straight in.
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
// must not be applied: the global --recommend-only flag is a master switch,
// the per-policy spec.rightSizing.recommendOnly field opts a single Policy
// into dry-run. Both injection paths (controller reconcile and admission
// webhook) must gate on this — never on the flag or the field alone.
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
