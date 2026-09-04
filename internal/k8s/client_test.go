package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Compile-level contract test only. The two contexts are the load-bearing part
// of the signature: the first is the cache's lifetime, the second bounds the
// blocking startup phase. Collapsing them back into one would let a caller hand
// the blocking wait a context its shutdown signal never cancels — how the
// webhook came to ignore SIGTERM for its whole (up to 2m) startup window.
//
// Exercising the real cache needs a live apiserver, so nothing here covers
// cache-sync blocking, cancellation, or read-through behavior.
var _ func(context.Context, context.Context, *runtime.Scheme) (client.Client, error) = NewCached

// cachedKinds drives the pre-warm loop, and the pre-warm is the only reason
// WaitForCacheSync means anything: cache.New registers no informers on its own,
// so an empty list makes the wait a silent no-op and pushes a cluster-wide LIST
// onto the first admission Get, inside that call's 2s timeout.
//
// The exact contents are pinned: dropping a kind the webhook reads per
// admission reintroduces the stall, and adding an owner-chain kind (see
// OwnerChainDisableFor) stands up an informer over every such object in the
// cluster — what DisableFor exists to prevent.
func TestCachedKindsArePreWarmed(t *testing.T) {
	got := cachedKinds()
	if len(got) != 3 {
		t.Fatalf("cachedKinds: got %d kinds, want 3 (Policy, WorkloadRecommendation, Namespace)", len(got))
	}
	if _, ok := got[0].(*sustainv1alpha1.Policy); !ok {
		t.Errorf("cachedKinds[0]: got %T, want *v1alpha1.Policy", got[0])
	}
	if _, ok := got[1].(*sustainv1alpha1.WorkloadRecommendation); !ok {
		t.Errorf("cachedKinds[1]: got %T, want *v1alpha1.WorkloadRecommendation", got[1])
	}
	if _, ok := got[2].(*corev1.Namespace); !ok {
		t.Errorf("cachedKinds[2]: got %T, want *corev1.Namespace", got[2])
	}
}

// stubCache implements just enough of cache.Cache to drive
// registerInformerWithRetry. Every other method is nil — calling one panics,
// which is the intended signal that this stub is only valid for GetInformer.
type stubCache struct {
	cache.Cache
	mu      sync.Mutex
	calls   int
	failFor int   // return NoKindMatch for this many initial calls
	hardErr error // when set, returned instead (non-retryable)
}

func (s *stubCache) GetInformer(
	_ context.Context, _ client.Object, _ ...cache.InformerGetOption,
) (cache.Informer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.hardErr != nil {
		return nil, s.hardErr
	}
	if s.calls <= s.failFor {
		return nil, &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "k8s.sustain.io", Kind: "WorkloadRecommendation"},
		}
	}
	return nil, nil
}

func (s *stubCache) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// On a fresh install the apiserver may not serve the kind yet: CRD
// establishment lags creation, and installCRDs=false lets the webhook start
// before its CRDs exist at all. Failing immediately turned that race into
// CrashLoopBackOff, whose backoff climbs to five minutes, and with
// failurePolicy=Fail a crash-looping webhook blocks pod creation in its scope.
func TestRegisterInformerWithRetry_WaitsForCRDEstablishment(t *testing.T) {
	c := &stubCache{failFor: 3}
	err := registerInformerWithRetry(context.Background(), c,
		&sustainv1alpha1.WorkloadRecommendation{}, time.Now().Add(time.Second), time.Millisecond)
	if err != nil {
		t.Fatalf("a CRD that becomes servable must be waited for, not failed on: %v", err)
	}
	if got := c.callCount(); got != 4 {
		t.Errorf("GetInformer called %d times, want 4 (3 not-yet-served + 1 success)", got)
	}
}

// Only "no matches for kind" resolves by waiting. An RBAC denial, an
// unreachable apiserver or a scheme gap will still be there in two minutes, and
// retrying them would replace a clear startup error with a long silence.
func TestRegisterInformerWithRetry_DoesNotRetryNonNoMatchErrors(t *testing.T) {
	c := &stubCache{hardErr: apierrors.NewForbidden(
		schema.GroupResource{Group: "k8s.sustain.io", Resource: "workloadrecommendations"},
		"", errors.New("no permission"))}

	start := time.Now()
	err := registerInformerWithRetry(context.Background(), c,
		&sustainv1alpha1.WorkloadRecommendation{}, time.Now().Add(time.Minute), 10*time.Millisecond)
	if err == nil {
		t.Fatal("a forbidden error must be returned, not retried")
	}
	if got := c.callCount(); got != 1 {
		t.Errorf("GetInformer called %d times for a non-retryable error, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s: a non-retryable error must not consume the retry budget", elapsed)
	}
}

// The wait is bounded so a genuinely absent CRD surfaces as a failed pod rather
// than one that never becomes Ready with no explanation.
func TestRegisterInformerWithRetry_GivesUpAfterTimeout(t *testing.T) {
	c := &stubCache{failFor: 1 << 30} // never becomes servable
	err := registerInformerWithRetry(context.Background(), c,
		&sustainv1alpha1.WorkloadRecommendation{}, time.Now().Add(50*time.Millisecond), time.Millisecond)
	if err == nil {
		t.Fatal("an absent CRD must eventually fail, not wait forever")
	}
	if !strings.Contains(err.Error(), "is the CRD installed?") {
		t.Errorf("error should tell the operator what to check, got: %v", err)
	}
}

// The wait before the HTTPS listener exists is the process's longest
// unresponsive stretch — up to crdWaitTimeout with nothing answering /healthz —
// so it must return the moment startCtx (the caller's shutdown signal) is
// cancelled rather than sleeping out its retry interval. The generous deadline
// and interval here are the point: neither may be what ends the call.
func TestRegisterInformerWithRetry_ReturnsPromptlyOnCancelledContext(t *testing.T) {
	c := &stubCache{failFor: 1 << 30} // never becomes servable
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := registerInformerWithRetry(ctx, c,
		&sustainv1alpha1.WorkloadRecommendation{}, time.Now().Add(crdWaitTimeout), 30*time.Second)
	if err == nil {
		t.Fatal("a cancelled context must end the wait with an error, not a nil client")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled so the caller can tell a shutdown "+
			"apart from an absent CRD, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s: a cancelled startup context must not wait out the retry "+
			"interval or the %s deadline", elapsed, crdWaitTimeout)
	}
}

// Nothing answers /healthz while the CRD-establishment wait lasts, so the chart
// gives the webhook a startupProbe whose budget exceeds crdWaitTimeout; without
// it the liveness probe kills the container at 40s on exactly the fresh-install
// race the wait exists for. This reads the chart rather than a copy of its
// numbers, so raising crdWaitTimeout alone fails here.
func TestCRDWaitFitsInsideChartStartupProbeBudget(t *testing.T) {
	const valuesPath = "../../charts/k8s-sustain/values.yaml"
	raw, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", valuesPath, err)
	}
	var values struct {
		Webhook struct {
			StartupProbe struct {
				InitialDelaySeconds int `json:"initialDelaySeconds"`
				PeriodSeconds       int `json:"periodSeconds"`
				FailureThreshold    int `json:"failureThreshold"`
			} `json:"startupProbe"`
		} `json:"webhook"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parsing %s: %v", valuesPath, err)
	}

	p := values.Webhook.StartupProbe
	if p.PeriodSeconds == 0 || p.FailureThreshold == 0 {
		t.Fatalf("webhook.startupProbe is missing from %s: liveness alone kills the container "+
			"long before the %s CRD wait completes", valuesPath, crdWaitTimeout)
	}
	budget := startupProbeBudget(p.InitialDelaySeconds, p.PeriodSeconds, p.FailureThreshold)
	if budget <= crdWaitTimeout {
		t.Errorf("webhook startupProbe budget is %s but crdWaitTimeout is %s: the container is "+
			"killed before the CRD-establishment wait can finish", budget, crdWaitTimeout)
	}
}

// values.yaml is not the only copy of these numbers: `helm upgrade
// --reuse-values` from a release predating webhook.startupProbe never picks up
// a newly added default, so the template carries its own fallback copy
// (webhook-deployment.yaml's mergeOverwrite dict) and that is what the upgrade
// path renders. Nothing tied it to crdWaitTimeout, so raising crdWaitTimeout
// and values.yaml together left every test green while the upgrade path was
// silently under budget. This pins the same relationship from the template side.
func TestCRDWaitFitsInsideChartTemplateStartupProbeDefaults(t *testing.T) {
	const templatePath = "../../charts/k8s-sustain/templates/webhook-deployment.yaml"
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading %s: %v", templatePath, err)
	}

	// The defaults live in a Go-template `mergeOverwrite (dict "k" v ...)`
	// literal that no YAML parser can see. Scoping the scan to that dict keeps
	// this from matching the liveness/readiness probes rendered further down.
	const marker = "$startupProbe := mergeOverwrite (dict"
	start := strings.Index(string(raw), marker)
	if start < 0 {
		t.Fatalf("%s no longer renders a template-side startupProbe default (%q not found): a "+
			"--reuse-values upgrade from a release predating webhook.startupProbe would get no "+
			"startup probe at all and CrashLoopBackOff on the %s CRD wait", templatePath, marker, crdWaitTimeout)
	}
	rest := string(raw)[start:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s: unterminated mergeOverwrite dict after %q", templatePath, marker)
	}
	dict := rest[:end]

	field := func(name string) int {
		t.Helper()
		m := regexp.MustCompile(`"` + name + `"\s+(\d+)`).FindStringSubmatch(dict)
		if m == nil {
			t.Fatalf("%s: template startupProbe default %q not found in %q", templatePath, name, dict)
		}
		v, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			t.Fatalf("%s: template startupProbe %q = %q: %v", templatePath, name, m[1], convErr)
		}
		return v
	}

	initial, period, failure := field("initialDelaySeconds"), field("periodSeconds"), field("failureThreshold")
	budget := startupProbeBudget(initial, period, failure)
	if budget <= crdWaitTimeout {
		t.Errorf("template-side startupProbe budget is %s but crdWaitTimeout is %s: a "+
			"--reuse-values upgrade renders THESE numbers, so the container is killed before the "+
			"CRD-establishment wait can finish", budget, crdWaitTimeout)
	}

	// Both copies exist so the two rendering paths behave identically. A drift
	// between them means install and upgrade give the webhook different budgets.
	const valuesPath = "../../charts/k8s-sustain/values.yaml"
	valuesRaw, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", valuesPath, err)
	}
	var values struct {
		Webhook struct {
			StartupProbe struct {
				InitialDelaySeconds int `json:"initialDelaySeconds"`
				PeriodSeconds       int `json:"periodSeconds"`
				FailureThreshold    int `json:"failureThreshold"`
			} `json:"startupProbe"`
		} `json:"webhook"`
	}
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatalf("parsing %s: %v", valuesPath, err)
	}
	v := values.Webhook.StartupProbe
	if v.InitialDelaySeconds != initial || v.PeriodSeconds != period || v.FailureThreshold != failure {
		t.Errorf("startupProbe defaults disagree: %s has %d/%d/%d, %s has %d/%d/%d "+
			"(initialDelaySeconds/periodSeconds/failureThreshold); a fresh install and a "+
			"--reuse-values upgrade would give the webhook different startup budgets",
			valuesPath, v.InitialDelaySeconds, v.PeriodSeconds, v.FailureThreshold,
			templatePath, initial, period, failure)
	}
}

// startupProbeBudget is how long a startupProbe suspends liveness/readiness
// before Kubernetes gives up on the container: the initial delay plus every
// allowed failure. This is the number that has to exceed crdWaitTimeout.
func startupProbeBudget(initialDelaySeconds, periodSeconds, failureThreshold int) time.Duration {
	return time.Duration(initialDelaySeconds+periodSeconds*failureThreshold) * time.Second
}

// runCtx is deliberately not the caller's shutdown signal, and a caller may
// legally pass context.Background(), so every error return is a place NewCached
// must stop the cache goroutine itself or leak a full informer set per failed
// startup — precisely the path a webhook retries on.
//
// The failure is forced with a scheme that knows none of the cached kinds, so
// GetInformer fails with no apiserver traffic. The error is not under test —
// the surviving goroutine is. The two returns after the cache starts need a
// live apiserver to reach; the package-wide goleak TestMain backstops those.
func TestNewCached_FailedStartupDoesNotLeakCacheGoroutine(t *testing.T) {
	t.Setenv("KUBECONFIG", writeStubKubeconfig(t))

	notLeaked := goleak.IgnoreCurrent()

	// runCtx is never cancelled here, exactly like a caller that hands over
	// context.Background(): the cleanup has to come from NewCached.
	c, err := NewCached(context.Background(), context.Background(), runtime.NewScheme())
	if err == nil {
		t.Fatalf("NewCached: want an error for a scheme missing every cached kind, got client %v", c)
	}
	if c != nil {
		t.Errorf("NewCached returned client %v alongside error %v; a failed construction must return no client", c, err)
	}

	goleak.VerifyNone(t, notLeaked)
}

// writeStubKubeconfig gives ctrl.GetConfigOrDie something to parse: finding
// nothing, it exits the process and takes the test binary with it. The server
// address is never dialled — these tests fail before any request is issued.
func writeStubKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const stub = `apiVersion: v1
kind: Config
clusters:
- name: stub
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: stub
  context:
    cluster: stub
    user: stub
current-context: stub
users:
- name: stub
  user: {}
`
	if err := os.WriteFile(path, []byte(stub), 0o600); err != nil {
		t.Fatalf("writing stub kubeconfig: %v", err)
	}
	return path
}
