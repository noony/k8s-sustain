package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"golang.org/x/sync/errgroup"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/httpx"
	"github.com/noony/k8s-sustain/internal/policymatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// maxAdmissionBodyBytes caps the AdmissionReview payload the webhook will
// decode. Pod specs in real clusters are kilobytes; 1 MiB is a generous
// ceiling that still defends against a malicious or malformed apiserver
// pushing an unbounded body.
const maxAdmissionBodyBytes = 1 << 20

// maxPolicyNameLen mirrors the Kubernetes resource-name limit (DNS subdomain).
// Anything longer cannot be a real Policy object.
const maxPolicyNameLen = 253

// apiCallTimeout bounds each individual Kubernetes API Get inside the
// admission path. The webhook's HTTP WriteTimeout (10s) is the outer ceiling;
// without per-call deadlines, one slow apiserver round-trip could eat the
// whole budget and leave Prometheus / cache fallback paths no time to run.
//
// 2s is a generous bound for cached etcd reads through the controller-runtime
// client (which uses an informer cache by default). Set high enough to absorb
// a brief apiserver hiccup, low enough to leave room for the Prometheus path
// and the AdmissionReview encode/decode on either side.
const apiCallTimeout = 2 * time.Second

// admissionTimeout caps the whole admit() handler. The apiserver's
// MutatingWebhookConfiguration timeout is 5s by default; we keep a 1s headroom
// for HTTP round-trip and JSON encode/decode so a stuck downstream (Prometheus
// or apiserver Get) cannot push us past the upstream deadline. Failing open
// inside the budget is strictly better than letting the apiserver time out and
// fall back to failurePolicy.
const admissionTimeout = 4 * time.Second

// isValidPolicyName guards against malformed annotation values flowing into
// Prometheus query selectors. Accepts only DNS-1123 subdomains up to 253 chars.
func isValidPolicyName(name string) bool {
	if name == "" || len(name) > maxPolicyNameLen {
		return false
	}
	return len(apivalidation.IsDNS1123Subdomain(name)) == 0
}

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

	// ExcludedNamespaces lists namespaces the webhook must never mutate.
	// Mirrors the controller's --excluded-namespaces flag so a workload in,
	// say, kube-system is left untouched by both components.
	ExcludedNamespaces []string

	// CacheStaleness bounds how old a WorkloadRecommendation can be before
	// the webhook refuses to use it as a fallback. Zero falls back to
	// DefaultCacheStaleness.
	CacheStaleness time.Duration
}

type jsonPatch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	httpx.LimitRequestBody(w, r, maxAdmissionBodyBytes)

	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		logger.Error(err, "failed to decode AdmissionReview")
		httpx.WriteError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if review.Request == nil {
		logger.Error(nil, "AdmissionReview has nil request")
		httpx.WriteError(w, http.StatusBadRequest, "missing request")
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
	ctx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()

	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name, "uid", req.UID)
	allow := &admissionv1.AdmissionResponse{Allowed: true}

	logger.V(1).Info("admit invoked", "operation", req.Operation, "kind", req.Kind.Kind)

	if len(req.Object.Raw) == 0 {
		// AdmissionRequest.Object is optional on the wire (e.g. DELETE has Raw
		// empty; a malformed apiserver review could also set it to nil). Bail
		// out fail-open rather than json.Unmarshal panicking on a nil slice.
		logger.V(1).Info("admission request has empty Object.Raw, allowing without injection")
		return allow
	}
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
	if !isValidPolicyName(policyName) {
		// A malformed annotation value would flow into Prometheus selector
		// strings (owner_name=%q etc.) — reject early to avoid wasted queries
		// and to refuse to act on a name we couldn't safely look up.
		logger.Info("pod has invalid policy annotation, allowing without injection", "policy", policyName)
		return allow
	}
	logger = logger.WithValues("policy", policyName)
	logger.V(1).Info("pod is annotated with policy")

	var policy sustainv1alpha1.Policy
	policyCtx, policyCancel := context.WithTimeout(ctx, apiCallTimeout)
	policyErr := h.Client.Get(policyCtx, types.NamespacedName{Name: policyName}, &policy)
	policyCancel()
	if policyErr != nil {
		if client.IgnoreNotFound(policyErr) == nil {
			logger.V(1).Info("policy not found, allowing pod")
			return allow // policy deleted — let pod through
		}
		logger.Error(policyErr, "failed to fetch policy")
		return allow
	}

	// Enforce the Policy's selector and the operator's excluded-namespaces
	// list. The controller applies the same predicate when listing
	// reconciliation targets (internal/controller/workload_target.go
	// filterTargets), so a workload outside the policy's scope is left alone
	// by both components.
	//
	// SelectorOK fails open: a malformed label selector is logged and
	// treated as non-matching, never as a pod denial.
	if _, selErr := policymatch.SelectorOK(policy.Spec.Selector.LabelSelector); selErr != nil {
		logger.Info("policy has invalid labelSelector; allowing pod without injection (fail-open)", "err", selErr)
		return allow
	}
	if !policymatch.Matches(&policy, req.Namespace, pod.Labels, h.ExcludedNamespaces) {
		logger.V(1).Info("pod does not match policy selector (namespace/labels) or is in excluded namespace; allowing without injection",
			"selectorNamespaces", policy.Spec.Selector.Namespaces,
			"excludedNamespaces", h.ExcludedNamespaces)
		return allow
	}

	ownerCtx, ownerCancel := context.WithTimeout(ctx, 2*apiCallTimeout)
	ownerKind, ownerName, err := workload.ResolvePodOwner(ownerCtx, h.Client, &pod)
	ownerCancel()
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
	mode := policy.Spec.RightSizing.Update.Types.ModeForKind(ownerKind)
	if mode == nil {
		logger.V(1).Info("policy does not configure this workload kind, skipping")
		return allow
	}
	logger.V(1).Info("policy configured for workload kind", "mode", *mode)

	containers, _ := workload.MergeContainersForRecommendation(
		pod.Spec.Containers, pod.Spec.InitContainers, policy.Spec.RightSizing.ExcludeInitContainers,
	)
	recs, err := h.buildRecommendations(ctx, &policy, req.Namespace, ownerKind, ownerName, containers)
	if err != nil {
		// Prometheus failed (timeout, network, or circuit breaker open).
		// Try the cached WorkloadRecommendation that the controller writes
		// after every successful reconcile. If it's fresh enough, inject
		// from cache; otherwise fall open with the original template.
		staleness := h.CacheStaleness
		if staleness == 0 {
			staleness = DefaultCacheStaleness
		}
		cacheCtx, cacheCancel := context.WithTimeout(ctx, apiCallTimeout)
		cached, cacheErr := h.fetchCachedRecommendations(cacheCtx, ownerKind, req.Namespace, ownerName, time.Now(), staleness)
		cacheCancel()
		if cacheErr != nil {
			logger.Error(cacheErr, "failed to read cached WorkloadRecommendation; falling open")
			return allow
		}
		if cached == nil {
			if errors.Is(err, promclient.ErrCircuitOpen) {
				logger.Info("prometheus circuit open and no fresh cache; allowing pod with template resources")
			} else {
				logger.Error(err, "failed to build recommendations and no fresh cache; allowing pod with template resources")
			}
			return allow
		}
		logger.Info("prometheus unavailable; serving cached recommendation",
			"containers", len(cached), "promErr", err.Error())
		recs = cached
	}

	// Always inject the latest recommendation regardless of mode.
	// The workload is annotated with a policy — the intent is to apply it.
	filtered := make(map[string]workload.ContainerRecommendation)
	for _, c := range pod.Spec.Containers {
		if rec, ok := recs[c.Name]; ok {
			filtered[c.Name] = rec
		}
	}
	if !policy.Spec.RightSizing.ExcludeInitContainers {
		for _, c := range pod.Spec.InitContainers {
			if rec, ok := recs[c.Name]; ok {
				filtered[c.Name] = rec
			}
		}
	}
	if len(filtered) == 0 {
		logger.V(1).Info("no recommendations match pod containers, allowing without injection",
			"podContainers", len(pod.Spec.Containers), "podInitContainers", len(pod.Spec.InitContainers), "recommendations", len(recs))
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

// buildRecommendations queries Prometheus for workload-level CPU/memory totals,
// recent OOM signal, and replica count (in parallel with autoscaler detection
// and the workload-creation lookup), then derives per-container per-pod
// recommendations. A per-pod floor is applied to protect against load
// imbalance. Autoscaler detection provides the MinReplicas fallback when
// Prometheus has no replica data (KEDA scale-to-zero, missing samples).
//
// The workload-age gate skips brand-new workloads (no usage history yet) so
// the webhook doesn't inject the hard-floored minimums that would crashloop
// the first pod. A recent OOM bypasses the gate so a replacement pod after
// an OOMKill gets a recommendation anchored on the kernel-observed peak
// rather than the too-small request that killed its predecessor.
//
// Unlike the controller, the webhook process has no access to the in-memory
// live OOM watcher — it relies solely on the Prometheus OOM signal.
func (h *Handler) buildRecommendations(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	ns, ownerKind, ownerName string,
	containers []corev1.Container,
) (map[string]workload.ContainerRecommendation, error) {
	logger := log.FromContext(ctx).WithValues("kind", ownerKind, "name", ownerName, "namespace", ns)
	rsCfg := policy.Spec.RightSizing.ResourcesConfigs

	// FetchWorkloadInputs, autoscaler.Detect, and the workload-creation lookup
	// all run in parallel. The replica divisor and OOM signal are produced by
	// FetchWorkloadInputs; autoscaler.MinReplicas only feeds the post-fetch
	// EffectiveReplicas computation, so overlapping these K8s + Prometheus
	// round-trips cuts admission wall time.
	var (
		inputs          *recommender.WorkloadInputs
		autoInfo        autoscaler.Info
		workloadCreated time.Time
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		v, err := recommender.FetchWorkloadInputs(gctx, h.PrometheusClient, ns, ownerKind, ownerName, rsCfg)
		if err != nil {
			return err
		}
		inputs = v
		return nil
	})
	g.Go(func() error {
		info, err := autoscaler.Detect(gctx, h.Client, ns, ownerKind, ownerName)
		if err != nil {
			logger.V(1).Info("autoscaler detection failed; using empty info", "err", err)
			autoInfo = autoscaler.Info{Kind: autoscaler.KindNone}
			return nil
		}
		autoInfo = info
		return nil
	})
	g.Go(func() error {
		workloadCreated = workload.GetWorkloadCreationTime(gctx, h.Client, ownerKind, ns, ownerName)
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	if recommender.ShouldSkipYoungWorkload(workloadCreated, inputs.HasRecentOOM()) {
		recommendationSkipped.WithLabelValues(ns, ownerKind, ownerName, "workload_too_young").Inc()
		logger.Info("skipping injection: workload too young",
			"age", time.Since(workloadCreated), "minAge", recommender.MinWorkloadAge)
		return map[string]workload.ContainerRecommendation{}, nil
	}

	replicas := recommender.EffectiveReplicas(inputs.MedianReplicas, autoInfo.MinReplicas)

	coordCfg := policy.Spec.RightSizing.AutoscalerCoordination
	recs := make(map[string]workload.ContainerRecommendation)
	for _, c := range containers {
		cpuTotal, hasCPU := inputs.CPUTotals[c.Name]
		memTotal, hasMem := inputs.MemTotals[c.Name]
		_, hasPeak := inputs.OOM.PeakMemoryBytes[c.Name]
		oom := recommender.NewOOMSignal(
			inputs.HasRecentOOM(),
			inputs.OOM.PeakMemoryBytes[c.Name],
			inputs.OOM.OOMLimitBytes[c.Name],
		)
		res := recommender.ComputeContainerRec(recommender.ContainerInputs{
			Container:   c,
			CPUTotal:    cpuTotal,
			HasCPU:      hasCPU,
			CPUFloor:    inputs.CPUFloors[c.Name],
			MemTotal:    memTotal,
			HasMemUsage: hasMem,
			MemFloor:    inputs.MemFloors[c.Name],
			Replicas:    replicas,
			OOM:         oom,
			HasOOMPeak:  hasPeak,
			AutoInfo:    autoInfo,
			RsCfg:       rsCfg,
			CoordCfg:    coordCfg,
		})
		if !res.HasData {
			continue
		}
		recs[c.Name] = res.Rec
	}
	return recs, nil
}

// buildPatches generates an RFC 6902 JSON Patch that sets resources on the
// containers (and init containers) listed in recs. Uses "add" which replaces
// any existing value at the path.
func buildPatches(pod *corev1.Pod, recs map[string]workload.ContainerRecommendation) ([]byte, error) {
	var patches []jsonPatch

	addPatches, err := patchesForContainers(pod.Spec.Containers, recs, "/spec/containers")
	if err != nil {
		return nil, err
	}
	patches = append(patches, addPatches...)

	addPatches, err = patchesForContainers(pod.Spec.InitContainers, recs, "/spec/initContainers")
	if err != nil {
		return nil, err
	}
	patches = append(patches, addPatches...)

	if len(patches) == 0 {
		return nil, nil
	}
	return json.Marshal(patches)
}

func patchesForContainers(cs []corev1.Container, recs map[string]workload.ContainerRecommendation, basePath string) ([]jsonPatch, error) {
	var patches []jsonPatch
	for i, c := range cs {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		// DeepCopy so the source pod object isn't mutated; ApplyRecommendation
		// writes the resource list in place.
		copyC := c.DeepCopy()
		if !workload.ApplyRecommendation(copyC, rec) {
			continue
		}
		newRes := copyC.Resources
		// ApplyRecommendation always seeds non-nil Limits; drop it back to nil
		// when empty so the generated patch leaves the wire identical to a pod
		// that never had limits set.
		if len(newRes.Limits) == 0 {
			newRes.Limits = nil
		}

		resJSON, err := json.Marshal(newRes)
		if err != nil {
			return nil, fmt.Errorf("marshaling resources for container %s: %w", c.Name, err)
		}
		patches = append(patches, jsonPatch{
			Op:    "add", // "add" replaces if path already exists
			Path:  fmt.Sprintf("%s/%d/resources", basePath, i),
			Value: resJSON,
		})
	}
	return patches, nil
}
