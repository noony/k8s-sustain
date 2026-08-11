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
// Kind is one of: Deployment, StatefulSet, DaemonSet, CronJob, Job, Rollout,
// Pod. Pod identifies a bare-pod identity formed via
// api/v1alpha1.OwnerNameAnnotation (see workload.GroupBarePods) — Name is the
// owner-name annotation value, not a real Kubernetes object name.
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

	// RecommendationSourceNoData means nothing has been computed for this
	// identity YET: it was looked up and produced nothing recommendable — too
	// young, no metrics, or the workload object is already gone.
	//
	// It is NOT terminal. The controller recomputes every
	// WorkloadRecommendation on its reconcile interval, so a nodata identity
	// is retried every cycle and converges on its own as soon as Prometheus
	// has enough history for it. Nothing ages the state out, and nothing has
	// to delete the object to give the identity another attempt.
	//
	// Consumers should read it as "no values to serve right now", not as "this
	// identity will never have values". The webhook does exactly that: it
	// admits the pod on its template resources and waits for a later cycle.
	RecommendationSourceNoData = "nodata"
)

// WorkloadRecommendationStatus is the observed recommendation, written by the
// controller after each successful reconcile and read by the webhook as its
// only source of recommendations at admission (the webhook never queries
// Prometheus itself).
type WorkloadRecommendationStatus struct {
	// ObservedAt is when the recommendation was last refreshed from
	// Prometheus. Consumers must check freshness before trusting the values.
	// +optional
	ObservedAt metav1.Time `json:"observedAt,omitempty"`

	// Source describes where the recommendation came from on this update.
	// One of RecommendationSourcePrometheus or RecommendationSourceNoData.
	// +optional
	Source string `json:"source,omitempty"`

	// Departed marks a recommendation the controller is deliberately RETAINING
	// for a workload identity that no longer exists — a completed and
	// TTL-deleted Job, a bare-pod group between runs — rather than one it has
	// merely failed to refresh.
	//
	// This distinction is what lets a consumer tell the two apart, and the
	// webhook depends on it. A departed identity IS still recomputed: the
	// controller's computation phase is driven by the WorkloadRecommendation
	// list rather than by a workload listing, so an identity with no object
	// behind it is queried on every reconcile interval like any other, and a
	// refresh that finds data writes a fresh ObservedAt (and clears this flag).
	//
	// The flag is for the case where that refresh finds NOTHING — the
	// identity's samples have aged out of the query window. The write rules
	// deliberately leave the previous values in place there rather than
	// overwriting a last-known-good with an empty status, and that means
	// ObservedAt is deliberately not bumped either: it would claim data that is
	// no longer there. So ObservedAt genuinely freezes, and within minutes the
	// recommendation is older than any staleness budget. Read as "stale" that
	// would mean a daily Job is admitted on template resources on every run but
	// its first — even though the retained recommendation is exactly the
	// last-known-good the retention window exists to preserve. This flag is
	// what makes the webhook serve it instead of dropping it.
	//
	// Set ONLY when the workload's absence has been positively confirmed (see
	// retainDepartedWLR); never on a failed existence check, which would let a
	// live workload bypass the freshness gate. Cleared whenever the identity is
	// seen in a target listing again (wlrcache.EnsureExists) or a real
	// recommendation is written for it (wlrcache.Upsert), so a stuck
	// controller — whose workloads stay in the target set — can never produce
	// this state.
	//
	// The flag carries no expiry of its own, but the reader applies one. The
	// per-policy sweep does delete the object once the retention window lapses
	// — while it runs: the sweep and the clearing above both sit inside
	// Reconcile, past an early return taken whenever the target listing fails
	// (revoked RBAC on one kind, an unreachable API group, a removed CRD). A
	// wedged controller therefore freezes the flag set, so the webhook checks
	// the age against its own --recommendation-retention rather than treating
	// "still exists" as proof of being inside the window
	// (internal/webhook.fetchRecommendations).
	// +optional
	Departed bool `json:"departed,omitempty"`

	// Containers maps container name → recommended resources.
	// +optional
	Containers map[string]ContainerRecommendation `json:"containers,omitempty"`

	// ObservedResources maps container name → the requests/limits the
	// container actually ran with, snapshotted each time the recommendation
	// is written (by the controller from the pod template, by the webhook
	// from the admitted pod). Lets the dashboard render current-vs-
	// recommended for workloads whose objects no longer exist.
	// +optional
	ObservedResources map[string]ObservedContainerResources `json:"observedResources,omitempty"`
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

// ObservedContainerResources is the requests/limits snapshot of one container
// at the last observation. Nil quantities mean the container had no value set
// for that field.
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
// for a single workload. The controller writes it after each reconcile; the
// webhook reads it as its only source of recommendations at admission — the
// webhook never queries Prometheus itself, so this cache is the primary
// path, not a fallback.
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
