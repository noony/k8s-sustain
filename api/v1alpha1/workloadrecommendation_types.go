package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRecommendationSpec identifies the workload this recommendation
// applies to, and the Policy that produced it.
type WorkloadRecommendationSpec struct {
	// WorkloadRef points at the workload these recommendations describe.
	// The triple (kind, namespace, name) is the identity. Stored on the spec
	// so it survives status edits and is searchable via field selectors.
	WorkloadRef WorkloadReference `json:"workloadRef"`

	// Policy is the name of the Policy whose configuration produced this
	// recommendation. Empty means the workload is no longer matched by any
	// Policy — controller will GC the object on its next sweep.
	// +optional
	Policy string `json:"policy,omitempty"`
}

// WorkloadReference uniquely identifies a workload within the cluster.
// Kind is one of: Deployment, StatefulSet, DaemonSet, CronJob, Job, Rollout.
type WorkloadReference struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;CronJob;Job;Rollout
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// WorkloadRecommendationStatus is the observed recommendation, written by the
// controller after each successful reconcile and read by the webhook as a
// fallback when Prometheus is unreachable.
type WorkloadRecommendationStatus struct {
	// ObservedAt is when the recommendation was last refreshed from
	// Prometheus. Consumers must check freshness before trusting the values.
	// +optional
	ObservedAt metav1.Time `json:"observedAt,omitempty"`

	// Source describes where the recommendation came from on this update.
	// "prometheus" — fresh data; "fallback" — propagated from a prior cache
	// when Prometheus itself was unavailable. Reserved for future use.
	// +optional
	Source string `json:"source,omitempty"`

	// Containers maps container name → recommended resources.
	// +optional
	Containers map[string]ContainerRecommendation `json:"containers,omitempty"`
}

// ContainerRecommendation is the per-container recommended resource set.
// All four quantities are optional: an unset value means "leave the
// corresponding spec entry alone" rather than "remove it".
//
// RemoveCPULimit / RemoveMemoryLimit encode the explicit "strip the limit"
// intent (Policy NoLimit). They are needed because nil Quantity alone cannot
// distinguish "leave alone" (KeepLimit / no strategy) from "remove".
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

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wlrec
// +kubebuilder:printcolumn:name="Workload",type="string",JSONPath=".spec.workloadRef.kind"
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.workloadRef.name"
// +kubebuilder:printcolumn:name="Policy",type="string",JSONPath=".spec.policy"
// +kubebuilder:printcolumn:name="ObservedAt",type="date",JSONPath=".status.observedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// WorkloadRecommendation is the cached output of the recommendation pipeline
// for a single workload. The controller writes it after each reconcile; the
// webhook reads it as a fallback when Prometheus is unavailable so admission
// can still inject last-known-good resources during outages.
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

func init() {
	SchemeBuilder.Register(&WorkloadRecommendation{}, &WorkloadRecommendationList{})
}
