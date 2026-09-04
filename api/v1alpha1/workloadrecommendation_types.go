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

	// Policy is the name of the Policy that produced this recommendation; empty means no Policy matches the workload anymore and the object will be garbage-collected.
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

// Values for WorkloadRecommendationStatus.Source.
const (
	// RecommendationSourcePrometheus marks a status computed from live
	// Prometheus data.
	RecommendationSourcePrometheus = "prometheus"

	// RecommendationSourceNoData means nothing recommendable was found yet.
	// It is not terminal: the identity is recomputed every reconcile cycle.
	RecommendationSourceNoData = "nodata"
)

// WorkloadRecommendationStatus is the observed recommendation, written by the
// controller and read by the webhook as its only recommendation source.
type WorkloadRecommendationStatus struct {
	// ObservedAt is when the recommendation was last refreshed from Prometheus.
	// +optional
	ObservedAt metav1.Time `json:"observedAt,omitempty"`

	// Source describes where the recommendation came from: "prometheus" or "nodata".
	// +optional
	Source string `json:"source,omitempty"`

	// Departed marks a recommendation retained for a workload identity that no longer exists (a TTL-deleted Job, a bare-pod group between runs), whose ObservedAt is therefore frozen.
	// +optional
	Departed bool `json:"departed,omitempty"`

	// Containers maps container name to recommended resources.
	// +optional
	Containers map[string]ContainerRecommendation `json:"containers,omitempty"`

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
