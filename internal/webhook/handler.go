package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/httpx"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// maxAdmissionBodyBytes caps the AdmissionReview payload the webhook decodes.
const maxAdmissionBodyBytes = 1 << 20

// maxPolicyNameLen is the Kubernetes DNS-subdomain name limit.
const maxPolicyNameLen = 253

// apiCallTimeout bounds each Kubernetes API Get inside the admission path, so
// one slow apiserver round-trip cannot eat the whole admission budget.
const apiCallTimeout = 2 * time.Second

// admissionTimeout caps the whole admit() handler. The apiserver's webhook
// timeout is 5s by default; failing open inside that budget beats letting the
// apiserver time out and fall back to failurePolicy.
const admissionTimeout = 4 * time.Second

// isValidPolicyName reports whether name is a DNS-1123 subdomain.
func isValidPolicyName(name string) bool {
	if name == "" || len(name) > maxPolicyNameLen {
		return false
	}
	return len(apivalidation.IsDNS1123Subdomain(name)) == 0
}

// Handler is the mutating admission handler. On Pod CREATE it injects the
// resources cached in the controller-written WorkloadRecommendation; it never
// queries Prometheus itself, so its load scales with workload count rather
// than pod churn.
type Handler struct {
	Client        client.Client
	RecommendOnly bool

	ExcludedNamespaces []string

	// CacheStaleness bounds how old a WorkloadRecommendation may be before the
	// webhook stops injecting from it. Zero falls back to DefaultCacheStaleness.
	CacheStaleness time.Duration

	// RecommendationRetention bounds the age of a Departed WorkloadRecommendation,
	// whose ObservedAt is frozen. It must mirror the controller's
	// --recommendation-retention. Zero falls back to DefaultRecommendationRetention.
	RecommendationRetention time.Duration

	// stubMu guards every stub field below, including stubWG.Add and
	// stubStopping: registering a goroutine and deciding no more may start must
	// be one atomic step or Shutdown can wait on a WaitGroup about to grow.
	stubMu        sync.Mutex
	stubRequested map[string]time.Time
	stubInflight  chan struct{}
	stubLastPrune time.Time

	// stubCtx is the parent of every detached stub goroutine; stubStop cancels it.
	stubCtx  context.Context
	stubStop context.CancelFunc

	stubStopping bool
	stubWG       sync.WaitGroup

	// ownerAnnCache and ownerRefCache bound the per-admission owner Gets on
	// clusters where a selector-less Policy covers every pod.
	ownerAnnCache ownerAnnotationsCache
	ownerRefCache ttlLRUCache[resolvedOwnerRef]

	// ownerAnnSF and ownerRefSF collapse a concurrent burst of cache misses
	// for the same key into one in-flight Get.
	ownerAnnSF singleflight.Group
	ownerRefSF singleflight.Group

	// sfJoinHook, if non-nil, is called with the singleflight key right after
	// DoChan registers a caller. Test-only; a per-Handler field rather than a
	// global so one test's stragglers cannot fire another test's barrier.
	sfJoinHook func(key string)
}

type jsonPatch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// allowWithLabelPatch returns an allow response carrying only the owner-name
// label-mirror patch, if any.
func allowWithLabelPatch(labelPatch *jsonPatch) *admissionv1.AdmissionResponse {
	if labelPatch == nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	b, err := json.Marshal([]jsonPatch{*labelPatch})
	if err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{Allowed: true, Patch: b, PatchType: &pt}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxAdmissionBodyBytes)

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

// admit processes a single AdmissionRequest. On any error it fails open and
// allows the pod unmutated.
func (h *Handler) admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	ctx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()

	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name, "uid", req.UID)
	allow := &admissionv1.AdmissionResponse{Allowed: true}

	logger.V(1).Info("admit invoked", "operation", req.Operation, "kind", req.Kind.Kind)

	if len(req.Object.Raw) == 0 {
		logger.V(1).Info("admission request has empty Object.Raw, allowing without injection")
		return allow
	}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		logger.Error(err, "failed to decode Pod")
		return allow
	}
	pod.Namespace = req.Namespace

	var resolvedOwner podOwner

	policyName := pod.Annotations[sustainv1alpha1.PolicyAnnotation]
	if pod.Annotations[sustainv1alpha1.OptOutAnnotation] == "true" {
		logger.V(1).Info("pod opts out, allowing without injection")
		return allow
	}
	if policyName == "" {
		optInCtx, optInCancel := context.WithTimeout(ctx, optInTimeout)
		name, level, owner, err := h.resolveOptIn(optInCtx, logger, &pod)
		optInCancel()
		if err != nil {
			logger.Error(err, "failed to resolve multi-level policy opt-in; allowing pod without injection")
			return allow
		}
		if name == "" {
			logger.V(1).Info("pod has no policy annotation at any level, allowing without injection")
			return allow
		}
		policyName, resolvedOwner = name, owner
		logger = logger.WithValues("optInLevel", level)
	}
	if !isValidPolicyName(policyName) {
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
			return allow
		}
		logger.Error(policyErr, "failed to fetch policy")
		return allow
	}

	// A malformed label selector is treated as non-matching, never as a denial.
	sel, selErr := policymatch.SelectorOK(policy.Spec.Selector.LabelSelector)
	if selErr != nil {
		logger.Info("policy has invalid labelSelector; allowing pod without injection (fail-open)", "err", selErr)
		return allow
	}
	if !policymatch.MatchesSelector(&policy, req.Namespace, pod.Labels, h.ExcludedNamespaces, sel) {
		logger.V(1).Info("pod does not match policy selector (namespace/labels) or is in excluded namespace; allowing without injection",
			"selectorNamespaces", policy.Spec.Selector.Namespaces,
			"excludedNamespaces", h.ExcludedNamespaces)
		return allow
	}

	// Mirror the owner-name annotation onto a label so kube-state-metrics
	// exposes it for the recording rules. Applied even in recommend-only mode.
	var labelPatch *jsonPatch
	if v, ok := pod.Annotations[sustainv1alpha1.OwnerNameAnnotation]; ok && len(apivalidation.IsValidLabelValue(v)) == 0 {
		if pod.Labels[sustainv1alpha1.OwnerNameAnnotation] != v {
			if pod.Labels == nil {
				if valJSON, mErr := json.Marshal(map[string]string{sustainv1alpha1.OwnerNameAnnotation: v}); mErr != nil {
					logger.Error(mErr, "failed to marshal owner-name label patch")
				} else {
					labelPatch = &jsonPatch{Op: "add", Path: "/metadata/labels", Value: valJSON}
				}
			} else {
				if valJSON, mErr := json.Marshal(v); mErr != nil {
					logger.Error(mErr, "failed to marshal owner-name label patch")
				} else {
					labelPatch = &jsonPatch{Op: "add", Path: "/metadata/labels/" + strings.ReplaceAll(sustainv1alpha1.OwnerNameAnnotation, "/", "~1"), Value: valJSON}
				}
			}
		}
	}

	ownerKind, ownerName := resolvedOwner.Kind, resolvedOwner.Name
	if !resolvedOwner.Resolved {
		ownerCtx, ownerCancel := context.WithTimeout(ctx, 2*apiCallTimeout)
		var err error
		ownerKind, ownerName, err = h.resolveCachedPodOwner(ownerCtx, &pod)
		ownerCancel()
		if err != nil {
			logger.Error(err, "failed to resolve owner kind")
			return allowWithLabelPatch(labelPatch)
		}
	}
	ownerKind, ownerName = workload.ApplyOwnerNameOverride(ownerKind, ownerName, pod.Annotations)
	if ownerKind == "" {
		logger.V(1).Info("standalone pod (no controller owner), skipping injection")
		return allowWithLabelPatch(labelPatch)
	}
	logger = logger.WithValues("ownerKind", ownerKind, "ownerName", ownerName)
	logger.V(1).Info("resolved pod owner")

	mode := policy.Spec.RightSizing.Update.Types.ModeForKind(ownerKind)
	if mode == nil {
		logger.V(1).Info("policy does not configure this workload kind, skipping")
		return allowWithLabelPatch(labelPatch)
	}
	logger.V(1).Info("policy configured for workload kind", "mode", *mode)

	staleness := h.CacheStaleness
	if staleness == 0 {
		staleness = DefaultCacheStaleness
	}
	cacheCtx, cacheCancel := context.WithTimeout(ctx, apiCallTimeout)
	recs, departed, err := h.fetchRecommendations(cacheCtx, ownerKind, req.Namespace, ownerName, time.Now(), staleness)
	cacheCancel()
	// RecommendationSourceTotal counts the read outcome, not whether a patch
	// is eventually emitted.
	if err != nil {
		if errors.Is(err, ErrRecommendationStale) {
			RecommendationSourceTotal.WithLabelValues(RecSourceStale).Inc()
			logger.V(1).Info("WorkloadRecommendation is stale; allowing pod with template resources")
			return allowWithLabelPatch(labelPatch)
		}
		if errors.Is(err, ErrRecommendationNoData) {
			RecommendationSourceTotal.WithLabelValues(RecSourceNoData).Inc()
			logger.V(1).Info("WorkloadRecommendation has no recommendable data; allowing pod with template resources")
			return allowWithLabelPatch(labelPatch)
		}
		RecommendationSourceTotal.WithLabelValues(RecSourceError).Inc()
		logger.Error(err, "failed to read WorkloadRecommendation; allowing pod with template resources")
		return allowWithLabelPatch(labelPatch)
	}
	if recs == nil {
		RecommendationSourceTotal.WithLabelValues(RecSourceMissing).Inc()
		// Only the missing path requests a stub; a stale object already exists.
		h.requestRecommendation(logger, req.Namespace, ownerKind, ownerName, policyName, pod.Spec.Containers, pod.Spec.InitContainers)
		logger.V(1).Info("no WorkloadRecommendation for workload; requested one, allowing pod with template resources")
		return allowWithLabelPatch(labelPatch)
	}
	if departed {
		RecommendationSourceTotal.WithLabelValues(RecSourceRetained).Inc()
	} else {
		RecommendationSourceTotal.WithLabelValues(RecSourceHit).Inc()
	}

	filtered := make(map[string]workload.ContainerRecommendation)
	addMatchingRecs(filtered, pod.Spec.Containers, recs)
	if !policy.Spec.RightSizing.ExcludeInitContainers {
		addMatchingRecs(filtered, pod.Spec.InitContainers, recs)
	}
	if len(filtered) == 0 {
		logger.V(1).Info("no recommendations match pod containers, allowing without injection",
			"podContainers", len(pod.Spec.Containers), "podInitContainers", len(pod.Spec.InitContainers), "recommendations", len(recs))
		return allowWithLabelPatch(labelPatch)
	}

	recommendOnly := policy.EffectiveRecommendOnly(h.RecommendOnly)

	if recommendOnly {
		source := "policy"
		if h.RecommendOnly {
			source = "flag"
		}
		logger.Info("recommend-only: would inject resources", "source", source, "containers", len(filtered), "recommendations", filtered)
		return allowWithLabelPatch(labelPatch)
	}

	patchBytes, err := buildPatches(&pod, filtered, labelPatch)
	if err != nil {
		logger.Error(err, "failed to build JSON patches")
		return allowWithLabelPatch(labelPatch)
	}
	if patchBytes == nil {
		logger.V(1).Info("no patch needed (recommendations match current pod spec)")
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

func addMatchingRecs(dst map[string]workload.ContainerRecommendation, containers []corev1.Container, recs map[string]workload.ContainerRecommendation) {
	for _, c := range containers {
		if rec, ok := recs[c.Name]; ok {
			dst[c.Name] = rec
		}
	}
}

// buildPatches generates an RFC 6902 JSON Patch setting resources on the
// containers listed in recs, plus the owner-name label patch when present.
func buildPatches(pod *corev1.Pod, recs map[string]workload.ContainerRecommendation, labelPatch *jsonPatch) ([]byte, error) {
	var patches []jsonPatch
	if labelPatch != nil {
		patches = append(patches, *labelPatch)
	}

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
		copyC := c.DeepCopy()
		if !workload.ApplyRecommendation(copyC, rec) {
			continue
		}
		newRes := copyC.Resources
		// ApplyRecommendation seeds non-nil Limits; drop it back to nil when
		// empty so the patch matches a pod that never had limits.
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
