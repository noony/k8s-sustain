package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// Handler is the HTTP handler for the mutating admission webhook.
// It intercepts Pod CREATE requests and injects resource requests/limits
// based on matching policies backed by Prometheus data.
// Both OnCreate and Ongoing policies are handled so that pods start with
// the latest recommendation immediately, without waiting for the controller
// to reconcile.
type Handler struct {
	Client           client.Client
	PrometheusClient *promclient.Client
	RecommendOnly    bool
}

type jsonPatch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		logger.Error(err, "failed to decode AdmissionReview")
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	resp := h.admit(r.Context(), review.Request)
	resp.UID = review.Request.UID
	review.Response = resp

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		logger.Error(err, "failed to encode AdmissionReview response")
	}
}

// admit processes a single AdmissionRequest. On any error it fails open
// (allows the pod) to avoid blocking the cluster.
func (h *Handler) admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name, "uid", req.UID)
	allow := &admissionv1.AdmissionResponse{Allowed: true}

	logger.V(1).Info("admit invoked", "operation", req.Operation, "kind", req.Kind.Kind)

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		logger.Error(err, "failed to decode Pod")
		return allow
	}
	pod.Namespace = req.Namespace

	policyName := pod.Annotations[sustainv1alpha1.PolicyAnnotation]
	if policyName == "" {
		logger.V(1).Info("pod has no policy annotation, allowing without injection")
		return allow // no annotation — pod is not managed by any policy
	}
	logger = logger.WithValues("policy", policyName)
	logger.V(1).Info("pod is annotated with policy")

	var policy sustainv1alpha1.Policy
	if err := h.Client.Get(ctx, types.NamespacedName{Name: policyName}, &policy); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.V(1).Info("policy not found, allowing pod")
			return allow // policy deleted — let pod through
		}
		logger.Error(err, "failed to fetch policy")
		return allow
	}

	ownerKind, ownerName, err := h.resolveOwner(ctx, &pod)
	if err != nil {
		logger.Error(err, "failed to resolve owner kind")
		return allow
	}
	if ownerKind == "" {
		logger.V(1).Info("standalone pod (no controller owner), skipping injection")
		return allow // standalone pod — no workload type to determine mode
	}
	logger = logger.WithValues("ownerKind", ownerKind, "ownerName", ownerName)
	logger.V(1).Info("resolved pod owner")

	// Act on both OnCreate and Ongoing policies so that pods always start
	// with the latest recommendation. Without this, Ongoing pods would start
	// with whatever the template currently has and only be resized later.
	mode := modeForKind(policy.Spec.RightSizing.Update.Types, ownerKind)
	if mode == nil {
		logger.V(1).Info("policy does not configure this workload kind, skipping")
		return allow
	}
	logger.V(1).Info("policy configured for workload kind", "mode", *mode)

	recs, err := h.buildRecommendations(ctx, &policy, req.Namespace, ownerKind, ownerName, pod.Spec.Containers)
	if err != nil {
		logger.Error(err, "failed to build recommendations")
		return allow
	}

	// Always inject the latest recommendation regardless of mode.
	// The workload is annotated with a policy — the intent is to apply it.
	filtered := make(map[string]workload.ContainerRecommendation)
	for _, c := range pod.Spec.Containers {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		filtered[c.Name] = rec
	}
	if len(filtered) == 0 {
		logger.V(1).Info("no recommendations match pod containers, allowing without injection",
			"podContainers", len(pod.Spec.Containers), "recommendations", len(recs))
		return allow
	}

	patchBytes, err := buildPatches(&pod, filtered)
	if err != nil {
		logger.Error(err, "failed to build JSON patches")
		return allow
	}
	if patchBytes == nil {
		logger.V(1).Info("no patch needed (recommendations match current pod spec)")
		return allow
	}

	if h.RecommendOnly {
		logger.Info("recommend-only: would inject resources", "containers", len(filtered), "recommendations", filtered)
		return allow
	}

	pt := admissionv1.PatchTypeJSONPatch
	logger.Info("injecting resources", "containers", len(filtered))
	logger.V(1).Info("injection details", "recommendations", filtered, "patchBytes", len(patchBytes))
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &pt,
	}
}

// resolveOwner walks a pod's ownerReferences to determine the top-level
// workload kind and name. Handles two indirect chains:
//   - Pod → ReplicaSet → Deployment
//   - Pod → Job → CronJob
//
// Returns ("", "", nil) for standalone pods.
func (h *Handler) resolveOwner(ctx context.Context, pod *corev1.Pod) (kind, name string, err error) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			var rs appsv1.ReplicaSet
			if err := h.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &rs); err != nil {
				return "", "", fmt.Errorf("getting replicaset %s: %w", ref.Name, err)
			}
			for _, rsRef := range rs.OwnerReferences {
				if rsRef.Controller != nil && *rsRef.Controller && rsRef.Kind == "Deployment" {
					return "Deployment", rsRef.Name, nil
				}
			}
			return "ReplicaSet", ref.Name, nil
		case "Job":
			var job batchv1.Job
			if err := h.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &job); err != nil {
				return "", "", fmt.Errorf("getting job %s: %w", ref.Name, err)
			}
			for _, jobRef := range job.OwnerReferences {
				if jobRef.Controller != nil && *jobRef.Controller && jobRef.Kind == "CronJob" {
					return "CronJob", jobRef.Name, nil
				}
			}
			return "Job", ref.Name, nil
		default:
			return ref.Kind, ref.Name, nil
		}
	}
	return "", "", nil
}

func modeForKind(ut sustainv1alpha1.UpdateTypes, kind string) *sustainv1alpha1.UpdateMode {
	switch kind {
	case "Deployment":
		return ut.Deployment
	case "StatefulSet":
		return ut.StatefulSet
	case "DaemonSet":
		return ut.DaemonSet
	case "CronJob":
		return ut.CronJob
	case "Job":
		return ut.Job
	case "Rollout":
		return ut.ArgoRollout
	}
	return nil
}

// buildRecommendations queries Prometheus for workload-level CPU/memory totals
// and replica count, then derives per-container per-pod recommendations.
// A per-pod floor is applied to protect against load imbalance.
// Autoscaler detection provides the MinReplicas fallback when Prometheus has
// no replica data (KEDA scale-to-zero, missing samples).
func (h *Handler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
) (map[string]workload.ContainerRecommendation, error) {
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	cpuQuantile := recommender.PercentileQuantile(rsCfg.CPU.Requests.Percentile)
	cpuWindow := recommender.ResourceWindow(rsCfg.CPU.Window)
	memQuantile := recommender.PercentileQuantile(rsCfg.Memory.Requests.Percentile)
	memWindow := recommender.ResourceWindow(rsCfg.Memory.Window)
	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)

	cpuTotals, err := h.PrometheusClient.QueryWorkloadCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("workload cpu query: %w", err)
	}
	memTotals, err := h.PrometheusClient.QueryWorkloadMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		return nil, fmt.Errorf("workload memory query: %w", err)
	}

	cpuFloors, err := h.PrometheusClient.QueryCPUByContainer(ctx, ns, ownerKind, ownerName, cpuQuantile, cpuWindow)
	if err != nil {
		logger.V(1).Info("per-pod cpu floor query failed; proceeding without floor", "err", err)
		cpuFloors = nil
	}
	memFloors, err := h.PrometheusClient.QueryMemoryByContainer(ctx, ns, ownerKind, ownerName, memQuantile, memWindow)
	if err != nil {
		logger.V(1).Info("per-pod memory floor query failed; proceeding without floor", "err", err)
		memFloors = nil
	}

	autoInfo, autoErr := autoscaler.Detect(ctx, h.Client, ns, ownerKind, ownerName)
	if autoErr != nil {
		logger.V(1).Info("autoscaler detection failed; using empty info", "err", autoErr)
		autoInfo = autoscaler.Info{Kind: autoscaler.KindNone}
	}
	medianReplicas, err := h.PrometheusClient.QueryReplicaCountMedian(ctx, ns, ownerKind, ownerName, cpuWindow)
	if err != nil {
		return nil, fmt.Errorf("replica count query: %w", err)
	}
	replicas := recommender.EffectiveReplicas(medianReplicas, autoInfo.MinReplicas)

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination

	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		var rec workload.ContainerRecommendation
		hasData := false

		if total, ok := cpuTotals[c.Name]; ok {
			perPod := recommender.PerPodFromTotal(total, replicas)
			perPod = recommender.ApplyFloor(perPod, cpuFloors[c.Name])
			rec.CPURequest = recommender.ComputeCPURequest(perPod, rsCfg.CPU.Requests)
			hasData = true
		}
		if total, ok := memTotals[c.Name]; ok {
			perPod := recommender.PerPodFromTotal(total, replicas)
			perPod = recommender.ApplyFloor(perPod, memFloors[c.Name])
			rec.MemoryRequest = recommender.ComputeMemoryRequest(perPod, rsCfg.Memory.Requests)
			hasData = true
		}

		if !hasData {
			continue
		}

		// Apply autoscaler coordination (overhead + replica budget) before
		// limits are derived, so limits track the adjusted requests.
		rec = recommender.ApplyCoordination(rec, coordCfg, autoInfo, rsCfg)

		// Re-derive limits from the (possibly) adjusted requests.
		if rec.CPURequest != nil {
			lr := recommender.ComputeLimit(rec.CPURequest, c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu(), rsCfg.CPU.Limits)
			rec.CPULimit = lr.Quantity
			rec.RemoveCPULimit = lr.Remove
		}
		if rec.MemoryRequest != nil {
			lr := recommender.ComputeLimit(rec.MemoryRequest, c.Resources.Requests.Memory(), c.Resources.Limits.Memory(), rsCfg.Memory.Limits)
			rec.MemoryLimit = lr.Quantity
			rec.RemoveMemoryLimit = lr.Remove
		}

		recs[c.Name] = rec
	}
	return recs, nil
}

// buildPatches generates an RFC 6902 JSON Patch that sets resources on the
// containers listed in recs. Uses "add" which replaces any existing value.
func buildPatches(pod *corev1.Pod, recs map[string]workload.ContainerRecommendation) ([]byte, error) {
	var patches []jsonPatch

	for i, c := range pod.Spec.Containers {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}

		newRes := c.Resources.DeepCopy()
		if newRes.Requests == nil {
			newRes.Requests = corev1.ResourceList{}
		}
		if newRes.Limits == nil {
			newRes.Limits = corev1.ResourceList{}
		}

		if rec.CPURequest != nil {
			newRes.Requests[corev1.ResourceCPU] = *rec.CPURequest
		}
		if rec.MemoryRequest != nil {
			newRes.Requests[corev1.ResourceMemory] = *rec.MemoryRequest
		}
		switch {
		case rec.RemoveCPULimit:
			delete(newRes.Limits, corev1.ResourceCPU)
		case rec.CPULimit != nil:
			newRes.Limits[corev1.ResourceCPU] = *rec.CPULimit
		}
		switch {
		case rec.RemoveMemoryLimit:
			delete(newRes.Limits, corev1.ResourceMemory)
		case rec.MemoryLimit != nil:
			newRes.Limits[corev1.ResourceMemory] = *rec.MemoryLimit
		}

		if len(newRes.Limits) == 0 {
			newRes.Limits = nil
		}

		resJSON, err := json.Marshal(newRes)
		if err != nil {
			return nil, fmt.Errorf("marshaling resources for container %s: %w", c.Name, err)
		}
		patches = append(patches, jsonPatch{
			Op:    "add", // "add" replaces if path already exists
			Path:  fmt.Sprintf("/spec/containers/%d/resources", i),
			Value: resJSON,
		})
	}

	if len(patches) == 0 {
		return nil, nil
	}
	return json.Marshal(patches)
}
