package webhook

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// reason says why an admission ends as it does. It is one of a closed set,
// stable and safe to log: a governance.Refusal, the RecommendationSourceTotal
// label of a verdict that injects nothing (missing, undecided, nodata,
// other-policy, stale), or one of the values below.
type reason string

const (
	reasonUndecodable              reason = "undecodable"
	reasonPanic                    reason = "panic"
	reasonGovernanceFailed         reason = "governance-failed"
	reasonRecommendationUnreadable reason = "recommendation-unreadable"
	// reasonNoContainerMatches: the Recommendation names none of the pod's
	// containers.
	reasonNoContainerMatches reason = "no-container-matches"
	reasonRecommendOnly      reason = "recommend-only"
	// reasonAtRecommendation: the pod already declares the resources the
	// Recommendation would set.
	reasonAtRecommendation reason = "at-recommendation"
	reasonInjected         reason = "injected"
)

type admission struct {
	reason reason
	// identity, optIn and level are the governance decision, as far as it
	// got before it refused the pod.
	identity promclient.WorkloadIdentity
	optIn    string
	level    governance.Level
	// policy and mode are empty when no Policy governs the pod.
	policy string
	mode   sustainv1alpha1.UpdateMode
	// source is the RecommendationSourceTotal label of the
	// WorkloadRecommendation read and verdict wlrcache's name for what the
	// read found; both are empty when nothing was read.
	source  string
	verdict string
	// held is the Recommendation the verdict injects, recs the part of it
	// that names the pod's containers.
	held map[string]workload.ContainerRecommendation
	recs map[string]workload.ContainerRecommendation
	// writes are the containers the response sets resources on; only an
	// injected admission has any.
	writes []workload.ContainerWrite
	// stub is the request for a Recommendation admit runs, nil when there is
	// nothing to ask for.
	stub *stubRequest
	err  error
}

// decide works out what admitting pod does. It writes nothing but the owner
// caches its governance reads fill, from singleflight leaders that may outlive
// it; the stub request, the one effect an admission can call for, is left in
// the admission it returns for admit to run. A pod is injected only when a
// Policy governs it and its identity's WorkloadRecommendation holds a
// Recommendation that Policy computed for it, fresh or retained.
func (h *Handler) decide(ctx context.Context, pod *corev1.Pod) admission {
	govCtx, govCancel := context.WithTimeout(ctx, governanceTimeout)
	g, err := governance.Decide(govCtx, governance.Pod(pod), admissionReader{h})
	govCancel()
	if err != nil {
		return admission{reason: reasonGovernanceFailed, err: err}
	}
	a := admission{identity: g.Identity, optIn: g.OptIn, level: g.Level}
	if !g.Governed() {
		a.reason = reason(g.Refusal)
		return a
	}
	policy := g.Policy
	a.policy, a.mode = policy.Name, g.Mode

	readCtx, readCancel := context.WithTimeout(ctx, apiCallTimeout)
	known, err := h.storedRecommendation(readCtx, g.Identity)
	readCancel()
	if err != nil {
		a.reason, a.source, a.err = reasonRecommendationUnreadable, RecSourceError, err
		return a
	}
	reading := wlrcache.Read(known, policy.Name, time.Now(), h.freshness())
	a.source, a.verdict, a.held = recommendationSource(reading.Verdict), reading.Verdict.String(), reading.Recs
	observed := wlrcache.BuildObservedResources(pod.Spec.Containers, pod.Spec.InitContainers)
	if wlrcache.ShouldRequest(known, policy.Name, observed) {
		a.stub = &stubRequest{known: known, identity: g.Identity, policy: policy.Name, observed: observed}
	}
	if !reading.Verdict.Injects() {
		a.reason = reason(a.source)
		return a
	}

	inj := workload.Inject(pod.Spec.Containers, pod.Spec.InitContainers, policy.Spec.RightSizing.ExcludeInitContainers, reading.Recs)
	a.recs = inj.Recs
	switch {
	case len(inj.Recs) == 0:
		a.reason = reasonNoContainerMatches
	case policy.EffectiveRecommendOnly(h.RecommendOnly):
		a.reason = reasonRecommendOnly
	case len(inj.Writes) == 0:
		a.reason = reasonAtRecommendation
	default:
		a.reason, a.writes = reasonInjected, inj.Writes
	}
	return a
}

// logAdmission logs one admission: an error for a failed read, info for what
// an operator acts on (an injection, a recommend-only dry run, an invalid
// selector), debug for every other way a pod is admitted unmutated.
func (h *Handler) logAdmission(logger logr.Logger, pod *corev1.Pod, a admission) {
	logger = logger.WithValues("reason", a.reason)
	if a.optIn != "" {
		logger = logger.WithValues("policy", a.optIn, "optInLevel", a.level)
	}
	if a.identity.OwnerKind != "" {
		logger = logger.WithValues("ownerKind", a.identity.OwnerKind, "ownerName", a.identity.OwnerName)
	}
	if a.mode != "" {
		logger = logger.WithValues("mode", a.mode)
	}
	if a.verdict != "" {
		logger = logger.WithValues("verdict", a.verdict)
	}
	switch a.reason {
	case reasonPanic:
		// Logged with its stack where it was recovered.
	case reasonGovernanceFailed:
		logger.Error(a.err, "failed to decide which Policy governs the pod; allowing pod without injection")
	case reasonRecommendationUnreadable:
		logger.Error(a.err, "failed to read WorkloadRecommendation; allowing pod with template resources")
	case reason(governance.SelectorInvalid):
		logger.Info("policy has invalid labelSelector; allowing pod without injection (fail-open)")
	case reasonNoContainerMatches:
		logger.V(1).Info("no recommendations match pod containers, allowing without injection",
			"podContainers", len(pod.Spec.Containers), "podInitContainers", len(pod.Spec.InitContainers), "recommendations", len(a.held))
	case reasonRecommendOnly:
		source := "policy"
		if h.RecommendOnly {
			source = "flag"
		}
		logger.Info("recommend-only: would inject resources", "source", source, "containers", len(a.recs), "recommendations", a.recs)
	case reasonInjected:
		logger.Info("injecting resources", "containers", len(a.writes))
		logger.V(1).Info("injection details", "recommendations", a.recs)
		for _, w := range a.writes {
			w.LogRaisedLimits(logger)
		}
	default:
		logger.V(1).Info("allowing pod without injection")
	}
}
