package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

func TestHandleWorkloadMetrics_BadWindow(t *testing.T) {
	srv := &Server{
		Policies:   governance.NewPolicyLister(nil),
		Clock:      clock.RealClock{},
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics?window=bogus", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadMetrics_BadStep(t *testing.T) {
	srv := &Server{
		Policies:   governance.NewPolicyLister(nil),
		Clock:      clock.RealClock{},
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics?step=999", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWorkloadMetrics_ReturnsAllKeys(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	d.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "wait-db"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	srv := &Server{Policies: governance.NewPolicyLister(nil), Clock: clock.RealClock{}, K8sClient: c, PromClient: &fakePromClient{}, Inputs: recommendertest.NewStaticInputs(), Logger: testLogger(t)}

	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics", nil),
		"default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	decodeEnvelopeData(t, rec.Body, &got)
	for _, k := range []string{"cpu", "memory", "resources", "running", "oomKills", "initContainers"} {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing %q key; got keys = %v", k, mapKeys(got))
		}
	}
	inits, _ := got["initContainers"].([]any)
	if len(inits) != 1 || inits[0] != "wait-db" {
		t.Errorf("initContainers = %v, want [wait-db]", inits)
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWorkloadMetricsAbsoluteRange(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(d).Build()
	prom := &fakePromClient{}
	srv := &Server{Policies: governance.NewPolicyLister(nil), Clock: clock.RealClock{}, K8sClient: c, PromClient: prom, Inputs: recommendertest.NewStaticInputs(), Logger: testLogger(t)}

	req := httptest.NewRequest(http.MethodGet,
		"/api/workloads/default/Deployment/web/metrics?from=1718000000&to=1718003600&step=5m", nil)
	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec, req, "default", "Deployment", "web")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(prom.capturedRanges) == 0 {
		t.Fatal("no range read")
	}
	for _, got := range prom.capturedRanges {
		if got.Start.Unix() != 1718000000 || got.End.Unix() != 1718003600 {
			t.Errorf("range = [%d,%d], want [1718000000,1718003600]", got.Start.Unix(), got.End.Unix())
		}
	}
}

// governedWeb is Deployment/web, container app, governed by Policy p, which
// reads CPU at p90 over 24h and memory at the defaults.
func governedWeb() (*sustainv1alpha1.Policy, *appsv1.Deployment) {
	mode := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode
	policy.Spec.RightSizing.ResourcesConfigs.CPU = sustainv1alpha1.ResourceConfig{
		Window:   "24h",
		Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: ptr.To[int32](90)},
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}}
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	return policy, d
}

type metricsPayload struct {
	CPU         map[string][]promclient.TimeValue `json:"cpu"`
	Signal      *usageSignal                      `json:"signal"`
	SignalError string                            `json:"signalError"`
	OOMKills    []promclient.OOMKill              `json:"oomKills"`
}

func getMetrics(t *testing.T, srv *Server, query string) metricsPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleWorkloadMetrics(rec,
		httptest.NewRequest(http.MethodGet, "/api/workloads/default/Deployment/web/metrics?"+query, nil),
		"default", "Deployment", "web")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got metricsPayload
	decodeEnvelopeData(t, rec.Body, &got)
	return got
}

// The detail payload carries both usage lines of each resource: the replica
// average, and what the usage signals read under the governing Policy's
// settings, read through the inputs port over the span a Simulation would
// chart, with the percentile and window they read at. The OOM kills come
// from that same read.
func TestHandleWorkloadMetrics_CarriesTheAverageTheSignalAndTheKills(t *testing.T) {
	from, to := time.Unix(1718000000, 0).UTC(), time.Unix(1718003600, 0).UTC()
	average := []promclient.TimeValue{{Timestamp: from, Value: 0.2}}
	read := func(cores, bytes float64, opts ...recommender.InputsOption) *recommender.WorkloadInputs {
		return recommender.NewInputs(append(opts,
			recommender.WithCPUUsage(promclient.ContainerValues{"app": cores}),
			recommender.WithMemoryUsage(promclient.ContainerValues{"app": bytes}))...)
	}
	inputs := recommendertest.NewStaticInputs().SetHistory(webKey, recommender.NewHistory([]recommender.HistoryPoint{
		{At: from, Inputs: read(0.5, 64)},
		{At: to, Inputs: read(0.7, 96, recommender.WithOOMKills(map[string]recommender.OOM{"app": {Kills: 1, KillsInStep: 1}}))},
	}))
	policy, d := governedWeb()
	srv := &Server{
		Policies:  governance.NewPolicyLister(nil),
		Clock:     clock.RealClock{},
		K8sClient: fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, d).Build(),
		PromClient: &fakePromClient{rangeByLabel: map[string]map[string][]promclient.TimeValue{
			perContainer("avg", promclient.MetricContainerCPUUsageByWorkloadRate1m, webKey): {"app": average},
		}},
		Inputs: inputs,
		Logger: testLogger(t),
	}

	got := getMetrics(t, srv, "from=1718000000&to=1718003600&step=30m")

	if !reflect.DeepEqual(got.CPU["app"], average) {
		t.Errorf("cpu = %v, want the replica average %v", got.CPU["app"], average)
	}
	want := &usageSignal{
		Policy:      "p",
		StepSeconds: 1800,
		CPU:         usageSignalSettings{Percentile: 90, Window: "24h"},
		Memory:      usageSignalSettings{Percentile: 95, Window: "168h"},
		Containers: map[string]usageSignalSeries{"app": {
			corev1.ResourceCPU:    []promclient.TimeValue{{Timestamp: from, Value: 0.5}, {Timestamp: to, Value: 0.7}},
			corev1.ResourceMemory: []promclient.TimeValue{{Timestamp: from, Value: 64}, {Timestamp: to, Value: 96}},
		}},
	}
	if !reflect.DeepEqual(got.Signal, want) || got.SignalError != "" {
		t.Errorf("signal = %+v, error %q; want %+v", got.Signal, got.SignalError, want)
	}
	if want := []promclient.OOMKill{{Timestamp: to, Container: "app"}}; !reflect.DeepEqual(got.OOMKills, want) {
		t.Errorf("kills = %v, want %v", got.OOMKills, want)
	}
	calls := inputs.HistoryCalls()
	if len(calls) != 1 || calls[0].Span != recommender.SpanOf(from, to, 30*time.Minute) ||
		!reflect.DeepEqual(calls[0].Resources, recommender.WithDefaults(policy.Spec.RightSizing.ResourcesConfigs)) {
		t.Errorf("history calls = %+v, want one over the chart range under p's resources, every default explicit", calls)
	}
	if len(inputs.Calls()) != 0 {
		t.Errorf("instant reads = %+v, want none: the detail page computes nothing", inputs.Calls())
	}
}

// A history that cannot be read costs the page its signal line, never the
// page: the average stands and the payload says why.
func TestHandleWorkloadMetrics_AnUnreadableSignalKeepsTheAverage(t *testing.T) {
	average := []promclient.TimeValue{{Timestamp: time.Unix(1718000000, 0).UTC(), Value: 0.2}}
	policy, d := governedWeb()
	srv := &Server{
		Policies:  governance.NewPolicyLister(nil),
		Clock:     clock.RealClock{},
		K8sClient: fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(policy, d).Build(),
		PromClient: &fakePromClient{rangeByLabel: map[string]map[string][]promclient.TimeValue{
			perContainer("avg", promclient.MetricContainerCPUUsageByWorkloadRate1m, webKey): {"app": average},
		}},
		Inputs: recommendertest.NewStaticInputs().FailHistory(webKey, errors.New("query processing would load too many samples into memory")),
		Logger: testLogger(t),
	}

	got := getMetrics(t, srv, "")

	if !reflect.DeepEqual(got.CPU["app"], average) || got.Signal != nil || !strings.Contains(got.SignalError, "too many samples") {
		t.Errorf("cpu %v, signal %+v, error %q; want the average and the history's error", got.CPU["app"], got.Signal, got.SignalError)
	}
}

// An identity no Policy governs, Conflicted or ungoverned, has no signal line:
// no Policy sizes from its usage. Its history is read under the recommender
// defaults for its kills alone.
func TestHandleWorkloadMetrics_NoSignalWithoutAGoverningPolicy(t *testing.T) {
	mode := sustainv1alpha1.UpdateModeOngoing
	policy := func(name string) client.Object {
		p := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.RightSizing.Update.Types.Deployment = &mode
		return p
	}
	member := func(name, policy string) client.Object {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.OwnerNameAnnotation: "web"}
		if policy != "" {
			d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] = policy
		}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
		return d
	}
	for name, objs := range map[string][]client.Object{
		"ungoverned": {member("web-blue", "")},
		"conflicted": {policy("p"), policy("q"), member("web-blue", "p"), member("web-green", "q")},
	} {
		t.Run(name, func(t *testing.T) {
			at := time.Unix(1718000000, 0).UTC()
			inputs := recommendertest.NewStaticInputs().SetHistory(webKey, recommender.NewHistory([]recommender.HistoryPoint{{
				At: at,
				Inputs: recommender.NewInputs(
					recommender.WithCPUUsage(promclient.ContainerValues{"app": 0.5}),
					recommender.WithOOMKills(map[string]recommender.OOM{"app": {Kills: 1, KillsInStep: 1}})),
			}}))
			srv := &Server{
				Policies:   governance.NewPolicyLister(nil),
				Clock:      clock.RealClock{},
				K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build(),
				PromClient: &fakePromClient{},
				Inputs:     inputs,
				Logger:     testLogger(t),
			}

			got := getMetrics(t, srv, "")

			if got.Signal != nil || got.SignalError != "" {
				t.Errorf("signal %+v, error %q; want none", got.Signal, got.SignalError)
			}
			if want := []promclient.OOMKill{{Timestamp: at, Container: "app"}}; !reflect.DeepEqual(got.OOMKills, want) {
				t.Errorf("kills = %v, want %v", got.OOMKills, want)
			}
			calls := inputs.HistoryCalls()
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].Resources, recommender.WithDefaults(sustainv1alpha1.ResourcesConfigs{})) {
				t.Errorf("history calls = %+v, want one under the recommender defaults", calls)
			}
		})
	}
}
