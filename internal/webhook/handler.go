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

// maxAdmissionBodyBytes caps the AdmissionReview payload the webhook will
// decode. Pod specs in real clusters are kilobytes; 1 MiB is a generous
// ceiling that still defends against a malicious or malformed apiserver
// pushing an unbounded body.
const maxAdmissionBodyBytes = 1 << 20

// maxPolicyNameLen mirrors the Kubernetes resource-name limit (DNS subdomain).
// Anything longer cannot be a real Policy object.
const maxPolicyNameLen = 253

// apiCallTimeout bounds each individual Kubernetes API Get inside the
// admission path (Policy lookup, owner resolution, WorkloadRecommendation
// read). The webhook's HTTP WriteTimeout (10s) is the outer ceiling; without
// per-call deadlines, one slow apiserver round-trip could eat the whole
// budget.
//
// 2s is a generous bound for direct apiserver reads through the uncached
// controller-runtime client (built with client.New). Set high enough to
// absorb a brief apiserver hiccup, low enough to leave room for the other
// calls in admit() and the AdmissionReview encode/decode on either side.
// Now that admission never talks to Prometheus, this bound will rarely be
// approached in practice — it remains as a backstop against a slow
// apiserver, not because it is load-bearing for a Prometheus round-trip
// anymore.
const apiCallTimeout = 2 * time.Second

// admissionTimeout caps the whole admit() handler. The apiserver's
// MutatingWebhookConfiguration timeout is 5s by default; we keep a 1s headroom
// for HTTP round-trip and JSON encode/decode so a stuck downstream (apiserver
// Get) cannot push us past the upstream deadline. Failing open inside the
// budget is strictly better than letting the apiserver time out and fall back
// to failurePolicy. As with apiCallTimeout, this backstop is now sized
// against the apiserver alone — Prometheus is no longer in the admission
// path at all.
const admissionTimeout = 4 * time.Second

// isValidPolicyName guards against malformed annotation values being used as
// a Policy object name in the apiserver Get below. Accepts only DNS-1123
// subdomains up to 253 chars.
func isValidPolicyName(name string) bool {
	if name == "" || len(name) > maxPolicyNameLen {
		return false
	}
	return len(apivalidation.IsDNS1123Subdomain(name)) == 0
}

// Handler is the HTTP handler for the mutating admission webhook.
// It intercepts Pod CREATE requests and injects resource requests/limits
// read from the WorkloadRecommendation the controller already computed and
// cached on its reconcile cadence. The webhook itself never queries
// Prometheus: doing so would make its query load scale with pod churn
// (mass scale-outs, node drains, crash loops) instead of workload count.
// Both OnCreate and Ongoing policies are handled so that pods start with
// the latest recommendation immediately, without waiting for the controller
// to reconcile.
type Handler struct {
	Client        client.Client
	RecommendOnly bool

	// ExcludedNamespaces lists namespaces the webhook must never mutate.
	// Mirrors the controller's --excluded-namespaces flag so a workload in,
	// say, kube-system is left untouched by both components.
	ExcludedNamespaces []string

	// CacheStaleness bounds how old a WorkloadRecommendation may be before
	// the webhook stops trusting it. This used to gate an emergency fallback
	// (used only when a live Prometheus query had already failed); now that
	// the WLR is the webhook's only recommendation source, it gates every
	// injection: once the controller falls this far behind — a stuck
	// reconcile loop, an outage, a huge backlog — the webhook stops injecting
	// from stale data and lets the pod through with template resources
	// instead. Zero falls back to DefaultCacheStaleness.
	CacheStaleness time.Duration

	// RecommendationRetention bounds the one path that waives CacheStaleness:
	// a WorkloadRecommendation the controller marked Departed, whose ObservedAt
	// is frozen by design (see fetchRecommendations). It must mirror the
	// controller's --recommendation-retention, because that is the window after
	// which the controller's sweep deletes the object — past it, the webhook is
	// serving something only a stuck controller could still be offering.
	//
	// The chart renders both from the single .Values.controller.recommendationRetention
	// key, so the two cannot drift in a chart-managed install. Zero falls back
	// to DefaultRecommendationRetention.
	RecommendationRetention time.Duration

	// Stub-request rate limiting. Lazily initialised (see
	// initStubStateLocked) because Handler is built as a plain struct
	// literal everywhere, so the zero value must work.
	stubMu        sync.Mutex
	stubRequested map[string]time.Time
	stubInflight  chan struct{}
	stubLastPrune time.Time
}

type jsonPatch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// allowWithLabelPatch returns an allow response carrying only the
// owner-name label-mirror patch (if any) — used by every early-return path
// from owner resolution onward, so a valid owner-name annotation always
// reaches Prometheus via kube-state-metrics even when the rest of admission
// has nothing else to inject.
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

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// Reject an oversized AdmissionReview before it lands in memory.
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
		// A malformed annotation value cannot be a real Policy object name —
		// reject early to avoid a wasted apiserver Get and to refuse to act
		// on a name we couldn't safely look up.
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
	// treated as non-matching, never as a pod denial. Compile the selector
	// once here and reuse it below, rather than calling Matches (which would
	// recompile the same selector a second time on this hot path).
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

	// Mirror a valid owner-name annotation onto a pod label so kube-state-metrics
	// (configured with metricLabelsAllowlist — see charts/k8s-sustain/values.yaml)
	// can expose it as kube_pod_labels for the recording rules to pick up.
	// Computed once, used by every return below from here on — independent of
	// RecommendOnly: the label is the only mechanism that makes the override
	// visible to Prometheus, and RecommendOnly's "never mutates the pod"
	// contract is about resources/limits, not this metadata label.
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

	ownerCtx, ownerCancel := context.WithTimeout(ctx, 2*apiCallTimeout)
	ownerKind, ownerName, err := workload.ResolvePodOwner(ownerCtx, h.Client, &pod)
	ownerCancel()
	if err != nil {
		logger.Error(err, "failed to resolve owner kind")
		return allowWithLabelPatch(labelPatch)
	}
	ownerKind, ownerName = workload.ApplyOwnerNameOverride(ownerKind, ownerName, pod.Annotations)
	if ownerKind == "" {
		logger.V(1).Info("standalone pod (no controller owner), skipping injection")
		return allowWithLabelPatch(labelPatch) // standalone pod — no workload type to determine mode
	}
	logger = logger.WithValues("ownerKind", ownerKind, "ownerName", ownerName)
	logger.V(1).Info("resolved pod owner")

	// Act on both OnCreate and Ongoing policies so that pods always start
	// with the latest recommendation. Without this, Ongoing pods would start
	// with whatever the template currently has and only be resized later.
	mode := policy.Spec.RightSizing.Update.Types.ModeForKind(ownerKind)
	if mode == nil {
		logger.V(1).Info("policy does not configure this workload kind, skipping")
		return allowWithLabelPatch(labelPatch)
	}
	logger.V(1).Info("policy configured for workload kind", "mode", *mode)

	// Read the recommendation the controller already computed and cached in
	// the WorkloadRecommendation on its reconcile cadence — the webhook
	// performs no computation and issues no Prometheus queries of its own.
	// A missing or stale WLR (see CacheStaleness) means we simply have
	// nothing to inject; fail open with the pod's template resources.
	staleness := h.CacheStaleness
	if staleness == 0 {
		staleness = DefaultCacheStaleness
	}
	cacheCtx, cacheCancel := context.WithTimeout(ctx, apiCallTimeout)
	recs, departed, err := h.fetchRecommendations(cacheCtx, ownerKind, req.Namespace, ownerName, time.Now(), staleness)
	cacheCancel()
	// RecommendationSourceTotal is incremented here, at the read outcome,
	// rather than at the eventual patch/no-patch return further down: it
	// answers "is the WLR pipeline keeping up", which is a property of this
	// read, not of what admit() goes on to do with a usable result (e.g.
	// recommend-only mode still counts as a "hit" even though it never
	// patches — the pipeline worked, the operator config just chose not to
	// apply it).
	if err != nil {
		if errors.Is(err, ErrRecommendationStale) {
			RecommendationSourceTotal.WithLabelValues(RecSourceStale).Inc()
			logger.V(1).Info("WorkloadRecommendation is stale; allowing pod with template resources")
			return allowWithLabelPatch(labelPatch)
		}
		if errors.Is(err, ErrRecommendationNoData) {
			// Already evaluated and found to have nothing recommendable. No
			// stub request: MarkNoData (wlrcache.go) is only ever reached
			// after the computation found a non-empty observed-resources
			// snapshot, so a nodata WLR already has one — createStub's
			// AlreadyExists path would have nothing to fill in even though
			// it now also attempts a snapshot write. Nothing here has to
			// prompt a retry either: nodata is not terminal, and the
			// computation phase recomputes this identity on every reconcile
			// interval for as long as the object exists.
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
		// Nothing has computed this workload identity yet. Ask the controller
		// to, by creating a stub WorkloadRecommendation it will fill in. Only
		// the missing path does this: on the stale path the object already
		// exists, so the Create is a guaranteed AlreadyExists — and createStub
		// no longer stops there, it follows up with a Get and possibly a
		// status Patch. That is up to three apiserver calls per admission to
		// discover there is nothing to do, since a stale object is one the
		// controller has already written and therefore already has an
		// observed-resources snapshot.
		//
		// This pod still starts on template resources; the stub is for the
		// next one. That is unavoidable — admission cannot wait for a
		// Prometheus query it is no longer allowed to make.
		h.requestRecommendation(logger, req.Namespace, ownerKind, ownerName, policyName, pod.Spec.Containers, pod.Spec.InitContainers)
		logger.V(1).Info("no WorkloadRecommendation for workload; requested one, allowing pod with template resources")
		return allowWithLabelPatch(labelPatch)
	}
	// Both outcomes injected a recommendation; they differ in its provenance —
	// "retained" is last-known-good for an identity that has departed, so its
	// age is bounded by --recommendation-retention rather than by staleness.
	if departed {
		RecommendationSourceTotal.WithLabelValues(RecSourceRetained).Inc()
	} else {
		RecommendationSourceTotal.WithLabelValues(RecSourceHit).Inc()
	}

	// Always inject the latest recommendation regardless of mode.
	// The workload is annotated with a policy — the intent is to apply it.
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

	// Dry-run is the OR of the global flag and the policy's own
	// spec.rightSizing.recommendOnly.
	recommendOnly := policy.EffectiveRecommendOnly(h.RecommendOnly)

	// Recommend-only records the recommendation but never mutates the pod, so
	// the response is always an allow with no patch. Short-circuit here to skip
	// buildPatches' per-container DeepCopy + json.Marshal — wasted work on the
	// pod-create hot path when the result is discarded.
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
		// Safe as plain allow: buildPatches folds labelPatch into patches, so
		// patchBytes is only nil when labelPatch was also nil.
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

// addMatchingRecs copies into dst the recommendations whose container name
// appears in containers, leaving non-matching recommendations out.
func addMatchingRecs(dst map[string]workload.ContainerRecommendation, containers []corev1.Container, recs map[string]workload.ContainerRecommendation) {
	for _, c := range containers {
		if rec, ok := recs[c.Name]; ok {
			dst[c.Name] = rec
		}
	}
}

// buildPatches generates an RFC 6902 JSON Patch that sets resources on the
// containers (and init containers) listed in recs, plus the owner-name
// label-mirror patch when present. Uses "add" which replaces any existing
// value at the path.
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
