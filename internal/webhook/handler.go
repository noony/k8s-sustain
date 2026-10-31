package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/httpx"
)

// maxAdmissionBodyBytes caps the AdmissionReview payload the webhook decodes.
const maxAdmissionBodyBytes = 1 << 20

// admissionTimeout caps the whole admit() handler. The apiserver's webhook
// timeout is 5s by default; failing open inside that budget beats letting the
// apiserver time out and fall back to failurePolicy.
const admissionTimeout = 4 * time.Second

// The admission budget is split by where its reads go. Only governance's
// owner chain reaches the apiserver, up to two Gets one after the other (the
// pod's ReplicaSet or Job, then its workload's own annotations); the Policies,
// the Namespace and the WorkloadRecommendation are informer-cache reads. So
// governance gets most of the budget, and the WorkloadRecommendation read
// keeps a reserve a slow owner Get cannot eat into.
const (
	recommendationReadTimeout = 1 * time.Second
	governanceTimeout         = admissionTimeout - recommendationReadTimeout
)

// apiCallTimeout bounds each owner-chain Get, which runs on a singleflight
// leader detached from the admissions waiting on it, so a wedged apiserver
// call cannot pin the leader.
const apiCallTimeout = 2 * time.Second

// Handler is the mutating admission handler. On Pod CREATE it injects the
// resources cached in the controller-written WorkloadRecommendation; it never
// queries Prometheus itself, so its load scales with workload count rather
// than pod churn.
type Handler struct {
	Client        client.Client
	RecommendOnly bool

	ExcludedNamespaces []string

	// CacheStaleness bounds how old a WorkloadRecommendation may be before the
	// webhook stops injecting from it. Zero falls back to
	// wlrcache.DefaultStaleness.
	CacheStaleness time.Duration

	// RecommendationRetention bounds the age of a retained (Departed or
	// Conflicted) WorkloadRecommendation, whose ObservedAt is frozen. It must
	// mirror the controller's --recommendation-retention. Zero falls back to
	// wlrcache.DefaultRetention.
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

	// policies spares an admission recompiling the Policies it lists.
	policies governance.PolicyLister

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

// admit decodes the pod, decides what admitting it does, runs the effects the
// decision calls for and responds. Every path admits the pod.
func (h *Handler) admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	ctx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()

	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name, "uid", req.UID)
	logger.V(1).Info("admit invoked", "operation", req.Operation, "kind", req.Kind.Kind)

	pod, err := decodePod(req)
	if err != nil {
		logger.Error(err, "failed to decode Pod; allowing it unmutated")
		return respond(admission{reason: reasonUndecodable})
	}
	return h.admitDecoded(ctx, logger, pod)
}

// admitDecoded turns a panic anywhere past decoding into an admission like
// any other. Left to the HTTP middleware it would answer 500, which hands the
// pod to the webhook's failurePolicy: under Fail, the pod is rejected.
func (h *Handler) admitDecoded(ctx context.Context, logger logr.Logger, pod *corev1.Pod) (resp *admissionv1.AdmissionResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			reportPanic(logger, panicLabelAdmit, rec)
			resp = respond(admission{reason: reasonPanic})
		}
	}()
	a := h.decide(ctx, pod)
	if a.source != "" {
		RecommendationSourceTotal.WithLabelValues(a.source).Inc()
	}
	if a.stub != nil {
		h.requestRecommendation(logger, *a.stub)
	}
	h.logAdmission(logger, pod, a)
	return respond(a)
}

func decodePod(req *admissionv1.AdmissionRequest) (*corev1.Pod, error) {
	if len(req.Object.Raw) == 0 {
		return nil, errors.New("admission request carries no object")
	}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return nil, err
	}
	pod.Namespace = req.Namespace
	return &pod, nil
}

// respond is the only constructor of an admission response, and it always
// allows the pod. It patches nothing but the resources of the containers an
// injected admission writes.
func respond(a admission) *admissionv1.AdmissionResponse {
	var patches []jsonPatch
	for _, w := range a.writes {
		path := "/spec/containers/"
		if w.Init {
			path = "/spec/initContainers/"
		}
		// Marshalling resource requirements cannot fail, and "add" replaces a
		// value already at the path.
		value, _ := json.Marshal(w.Resources)
		patches = append(patches, jsonPatch{Op: "add", Path: path + strconv.Itoa(w.Index) + "/resources", Value: value})
	}
	if len(patches) == 0 {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	b, err := json.Marshal(patches)
	if err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{Allowed: true, Patch: b, PatchType: &pt}
}

// reportPanic logs a recovered panic with its stack and counts it on
// PanicTotal under op, which must be a bounded constant.
func reportPanic(logger logr.Logger, op string, rec any, keysAndValues ...any) {
	logger.Error(nil, "recovered panic", append([]any{"operation", op, "panic", fmt.Sprint(rec), "stack", string(debug.Stack())}, keysAndValues...)...)
	PanicTotal.WithLabelValues(op).Inc()
}
