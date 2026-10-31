package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

// newCoordinatedSimulateServer builds the owner-name identity Deployment/web
// of members web-blue and web-green, governed by policy "p" with autoscaler
// coordination enabled, reporting one core of usage. An HPA at 50% CPU
// targets web-green, never the identity's own name, the way the controller
// reads it. Coordinated, that is 1 * 110/50 = 2.2 cores.
func newCoordinatedSimulateServer(t *testing.T) *Server {
	t.Helper()
	mode := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Deployment = &mode
	policy.Spec.RightSizing.AutoscalerCoordination.Enabled = true

	objs := []client.Object{policy}
	for _, name := range []string{"web-blue", "web-green"} {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		d.Spec.Template.Annotations = map[string]string{
			sustainv1alpha1.PolicyAnnotation:    "p",
			sustainv1alpha1.OwnerNameAnnotation: "web",
		}
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}
		objs = append(objs, d)
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-green"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web-green"},
			MaxReplicas:    5,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](50)},
				},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(append(objs, hpa)...).Build()
	inputs := recommendertest.NewStaticInputs().Set(
		promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"},
		recommender.WithCPUUsage(promclient.ContainerValues{"main": 1}))
	return &Server{K8sClient: c, PromClient: &fakePromClient{}, Inputs: inputs, Logger: testLogger(t)}
}

// The override merges onto the governing Policy: what it leaves alone is the
// Policy's, and the effective settings it returns have every default
// explicit.
func TestRunSimulation_OverrideMergesOntoTheGoverningPolicy(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		Override: json.RawMessage(`{"resourcesConfigs":{"cpu":{"requests":{"headroom":10}}}}`),
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; got != "2420m" {
		t.Errorf("cpu = %q, want 2420m: 1 core with 10%% headroom, coordinated as the Policy says", got)
	}
	cpu := res.Effective.ResourcesConfigs.CPU
	if res.Policy != "p" || !res.Effective.AutoscalerCoordination.Enabled || *cpu.Requests.Headroom != 10 ||
		*cpu.Requests.Percentile != 95 || cpu.Window != "168h" || !cpu.Limits.KeepLimit {
		t.Errorf("policy %q, effective %+v; want p's coordination, the override's headroom and the defaults", res.Policy, res.Effective)
	}
	if _, ok := recommender.TracedCoordination(res.Trace["main"].CPU); !ok {
		t.Errorf("trace = %+v, want the Simulation's own, with its coordination stage", res.Trace["main"])
	}
}

// A Simulation can start from any Policy instead of the governing one.
func TestRunSimulation_StartsFromTheChosenPolicy(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	uncoordinated := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "flat"}}
	if err := srv.K8sClient.Create(context.Background(), uncoordinated); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web", Policy: "flat",
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if got := res.Containers["main"].CPURequest; res.Policy != "flat" || got != "1" {
		t.Errorf("policy %q, cpu %q; want flat's uncoordinated 1 core", res.Policy, got)
	}
}

var webKey = promclient.WorkloadIdentity{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"}

// A Simulation's history is its computation at each instant of its chart
// range, the range's step coarsened to keep within the points a chart holds:
// the recommended request and limit from every signal's values as of the
// instant, the OOM floor only from the kill on and coordinated with today's
// HPA, next to the busiest replica's usage the request was computed from,
// what the pods ran with, and the kills. That a Simulation without an
// override equals the controller's Recommendation is held by the
// controller's TestSimulationRecommendsWhatTheControllerStores.
func TestRunSimulation_HistoryIsTheComputationAtEachInstant(t *testing.T) {
	const mib = 1 << 20
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	t0, t1 := at.Add(-2*time.Hour), at.Add(-time.Hour)
	srv := newCoordinatedSimulateServer(t)
	srv.Clock = clocktesting.NewFakePassiveClock(at)
	usage := func(cores float64) []recommender.InputsOption {
		return []recommender.InputsOption{
			recommender.WithCPUUsage(promclient.ContainerValues{"main": cores}),
			recommender.WithMemoryUsage(promclient.ContainerValues{"main": 64 * mib}),
		}
	}
	killed := append(usage(1), recommender.WithOOMKills(map[string]recommender.OOM{"main": {Kills: 1, PeakBytes: 200 * mib, HasPeak: true, KillsInStep: 1}}))
	kill := promclient.OOMKill{Timestamp: at, Container: "main"}
	inputs := recommendertest.NewStaticInputs().Set(webKey, killed...).SetHistory(webKey, recommender.NewHistory([]recommender.HistoryPoint{
		{At: t0, Inputs: recommender.NewInputs(usage(0.5)...)},
		{At: t1, Inputs: recommender.NewInputs(usage(1)...)},
		{At: at, Inputs: recommender.NewInputs(killed...)},
	}))
	srv.Inputs = inputs
	running := []promclient.TimeValue{{Timestamp: t0, Value: 0.25}, {Timestamp: at, Value: 0.5}}
	srv.PromClient = &fakePromClient{rangeByLabel: map[string]map[string][]promclient.TimeValue{
		perContainer("max", promclient.MetricContainerCPURequestsByWorkloadCores, webKey): {"main": running},
		perContainer("max", promclient.MetricContainerCPULimitsByWorkloadCores, webKey):   {"main": running},
	}}

	res, err := srv.runSimulation(context.Background(), simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web", Window: "2h", Step: "30m",
		Override: json.RawMessage(`{"resourcesConfigs":{"memory":{"limits":{"requestsLimitsRatio":2}}}}`),
	})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}

	if res.History == nil || res.HistoryError != "" {
		t.Fatalf("history = %+v, error %q; want the history", res.History, res.HistoryError)
	}
	if calls := inputs.HistoryCalls(); len(calls) != 1 || !calls[0].Span.End.Equal(at) || calls[0].Span.Step != 30*time.Minute ||
		!calls[0].Span.Start.Equal(t0) || !reflect.DeepEqual(calls[0].Resources, res.Effective.ResourcesConfigs) {
		t.Errorf("history calls = %+v, want one over the 2h back from the Simulation's instant, every 30m, under its effective resources", calls)
	}
	if res.History.StepSeconds != 1800 {
		t.Errorf("stepSeconds = %d, want 1800", res.History.StepSeconds)
	}
	points := func(vs ...float64) []promclient.TimeValue {
		out := make([]promclient.TimeValue, len(vs))
		for i, v := range vs {
			out[i] = promclient.TimeValue{Timestamp: []time.Time{t0, t1, at}[i], Value: v}
		}
		return out
	}
	main := res.History.Containers["main"]
	for what, tc := range map[string]struct{ got, want []promclient.TimeValue }{
		"cpu usage":                  {main.CPU.Usage, points(0.5, 1, 1)},
		"cpu recommended request":    {main.CPU.RecommendedRequest, points(1.1, 2.2, 2.2)},
		"cpu recommended limit":      {main.CPU.RecommendedLimit, nil},
		"cpu running request":        {main.CPU.RunningRequest, running},
		"cpu running limit":          {main.CPU.RunningLimit, running},
		"memory usage":               {main.Memory.Usage, points(64*mib, 64*mib, 64*mib)},
		"memory recommended request": {main.Memory.RecommendedRequest, points(64*mib, 64*mib, 200*mib)},
		"memory recommended limit":   {main.Memory.RecommendedLimit, points(128*mib, 128*mib, 400*mib)},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", what, tc.got, tc.want)
		}
	}
	if !reflect.DeepEqual(res.History.OOMKills, []promclient.OOMKill{kill}) {
		t.Errorf("kills = %v, want %v", res.History.OOMKills, kill)
	}
}

// A history that cannot be read does not fail the Simulation: its numbers
// come back with the reason, which the chart shows in its place.
func TestHandleSimulate_AnUnreadableHistoryKeepsTheNumbers(t *testing.T) {
	srv := newCoordinatedSimulateServer(t)
	srv.Inputs = recommendertest.NewStaticInputs().
		Set(webKey, recommender.WithCPUUsage(promclient.ContainerValues{"main": 1})).
		FailHistory(webKey, errors.New("query processing would load too many samples into memory"))
	rec := httptest.NewRecorder()

	srv.handleSimulate(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web"})))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Containers   map[string]simulationContainerResult `json:"containers"`
		History      *simulationHistory                   `json:"history"`
		HistoryError string                               `json:"historyError"`
	}
	decodeEnvelopeData(t, rec.Body, &got)
	if got.Containers["main"].CPURequest != "2200m" || got.History != nil || !strings.Contains(got.HistoryError, "too many samples") {
		t.Errorf("containers %+v, history %+v, error %q; want the numbers and the history's error", got.Containers, got.History, got.HistoryError)
	}
}
