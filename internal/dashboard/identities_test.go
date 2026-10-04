package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func ptrMode(m sustainv1alpha1.UpdateMode) *sustainv1alpha1.UpdateMode { return &m }

// deploymentWithOwnerName opts into policy "p" and, when ownerName is set,
// joins that owner-name identity. Its one container is named after it.
func deploymentWithOwnerName(ns, name, ownerName string, created time.Time) *appsv1.Deployment {
	return deploymentOptingInto("p", ns, name, ownerName, created)
}

func deploymentOptingInto(policy, ns, name, ownerName string, created time.Time) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: policy},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name}}},
			},
		},
	}
	if ownerName != "" {
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = ownerName
	}
	return d
}

func deploymentPolicy(name string) *sustainv1alpha1.Policy {
	p := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.RightSizing.Update.Types.Deployment = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	return p
}

func identityServer(t *testing.T, health memHealthSignals, objs ...client.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).Build()
	return &Server{K8sClient: c, Logger: testLogger(t), PromClient: &fakePromClient{}, Health: health, Inputs: recommendertest.NewStaticInputs()}
}

type listedRow struct {
	Name                string    `json:"name"`
	RiskState           riskState `json:"riskState"`
	Departed            bool      `json:"departed"`
	Automated           bool      `json:"automated"`
	PolicyName          string    `json:"policyName"`
	ConflictingPolicies []string  `json:"conflictingPolicies"`
	Containers          []struct {
		Name string `json:"name"`
	} `json:"containers"`
}

type listedPage struct {
	Items  []listedRow    `json:"items"`
	Counts workloadCounts `json:"counts"`
}

func listAll(t *testing.T, srv *Server, query string) listedPage {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var page listedPage
	decodeEnvelopeData(t, rec.Body, &page)
	return page
}

func listPolicy(t *testing.T, srv *Server, policy string) []listedRow {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handlePolicyWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/policies/"+policy+"/workloads", nil), policy)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var page listedPage
	decodeEnvelopeData(t, rec.Body, &page)
	return page.Items
}

func detail(t *testing.T, srv *Server, ns, kind, name string, into any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/"+ns+"/"+kind+"/"+name, nil), ns, kind, name)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	decodeEnvelopeData(t, rec.Body, into)
}

func rowNames(rows []listedRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// An owner-name group is one identity whose containers are the union of its
// members', dated by its earliest member: a Simulation of a group whose newest
// member is minutes old is not Too young when an older member has history.
func TestOwnerNameGroupShowsUnionContainersAndEarliestAge(t *testing.T) {
	srv := identityServer(t, memHealthSignals{},
		deploymentPolicy("p"),
		deploymentWithOwnerName("prod", "api-blue", "api", time.Now().Add(-48*time.Hour)),
		deploymentWithOwnerName("prod", "api-green", "api", time.Now().Add(-time.Minute)))
	srv.Inputs = recommendertest.NewStaticInputs().Set(identity("prod", "Deployment", "api"), &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"api-blue": 0.1, "api-green": 0.1},
	})

	page := listAll(t, srv, "")
	if len(page.Items) != 1 {
		t.Fatalf("rows = %v, want one owner-name identity", rowNames(page.Items))
	}
	var containers []string
	for _, c := range page.Items[0].Containers {
		containers = append(containers, c.Name)
	}
	if !slices.Equal(containers, []string{"api-green", "api-blue"}) {
		t.Errorf("containers = %v, want the union of both members", containers)
	}

	res, err := srv.runSimulation(context.Background(), simulateRequest{Namespace: "prod", OwnerKind: "Deployment", OwnerName: "api"})
	if err != nil {
		t.Fatalf("runSimulation: %v", err)
	}
	if res.TooYoung {
		t.Error("TooYoung: the identity's age must run from its earliest member, not its newest")
	}
	if len(res.Resources) != 2 {
		t.Errorf("simulation resources = %v, want both members' containers", res.Resources)
	}
}

// A finished standalone Job is no live member: its identity is listed as
// Departed, under the Policy its WorkloadRecommendation names.
func TestFinishedJobIsListedAsDeparted(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.RightSizing.Update.Types.Job = ptrMode(sustainv1alpha1.UpdateModeOngoing)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly"}}
	job.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "main"}}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	srv := identityServer(t, memHealthSignals{}, policy, job, retainedWLR("p", "batch", "Job", "nightly"))

	page := listAll(t, srv, "")
	if len(page.Items) != 1 {
		t.Fatalf("rows = %v, want the Job's one identity", rowNames(page.Items))
	}
	row := page.Items[0]
	if !row.Departed || !row.Automated || row.PolicyName != "p" {
		t.Errorf("departed/automated/policy = %v/%v/%q, want true/true/p", row.Departed, row.Automated, row.PolicyName)
	}
	if rows := listPolicy(t, srv, "p"); len(rows) != 1 || !rows[0].Departed {
		t.Errorf("p's rows = %+v, want the Job listed as Departed", rows)
	}
}

// Members opting into different Policies make one Conflicted identity: it is
// listed once, in the Conflicted state, under neither Policy.
func TestConflictedIdentityIsListedOnceUnderNoPolicy(t *testing.T) {
	now := time.Now()
	srv := identityServer(t, memHealthSignals{
		identity("prod", "Deployment", "api"): {Blocked: &blockedSignal{Reason: "patch", Attempts: 2}},
	},
		deploymentPolicy("p"), deploymentPolicy("q"),
		deploymentOptingInto("p", "prod", "api-blue", "api", now),
		deploymentOptingInto("q", "prod", "api-green", "api", now),
		deploymentOptingInto("p", "prod", "web", "", now))

	page := listAll(t, srv, "")
	if got := rowNames(page.Items); !slices.Equal(got, []string{"api", "web"}) {
		t.Fatalf("rows = %v, want api once and web", got)
	}
	api := page.Items[0]
	if api.RiskState != riskConflicted || api.Automated || api.PolicyName != "" {
		t.Errorf("api: risk/automated/policy = %q/%v/%q, want conflicted/false/none", api.RiskState, api.Automated, api.PolicyName)
	}
	if !slices.Equal(api.ConflictingPolicies, []string{"p", "q"}) {
		t.Errorf("api: conflicting policies = %v, want [p q]", api.ConflictingPolicies)
	}
	if want := (workloadCounts{Total: 2, Automated: 1, Conflicted: 1}); page.Counts != want {
		t.Errorf("counts = %+v, want %+v", page.Counts, want)
	}
	if got := rowNames(listAll(t, srv, "?risk=conflicted").Items); !slices.Equal(got, []string{"api"}) {
		t.Errorf("risk=conflicted rows = %v, want [api]", got)
	}

	if got := rowNames(listPolicy(t, srv, "p")); !slices.Equal(got, []string{"web"}) {
		t.Errorf("p's rows = %v, want only web", got)
	}
	if got := listPolicy(t, srv, "q"); len(got) != 0 {
		t.Errorf("q's rows = %v, want none", rowNames(got))
	}

	var got workloadDetailResponse
	detail(t, srv, "prod", "Deployment", "api", &got)
	if got.RiskState != riskConflicted || got.Automated || !slices.Equal(got.ConflictingPolicies, []string{"p", "q"}) {
		t.Errorf("detail = %+v, want Conflicted between p and q", got)
	}
}

// The detail page shows the Recommendation the WorkloadRecommendation stores,
// how it was derived and the outcome of the controller's last pass; it
// computes nothing.
func TestWorkloadDetailShowsStoredRecommendation(t *testing.T) {
	cpu, mem := resource.MustParse("250m"), resource.MustParse("128Mi")
	percentile := resource.MustParse("227500u")
	stored := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: wlrcache.Name("Deployment", "web")},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "prod", Name: "web"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
			Outcome:    sustainv1alpha1.OutcomeNoData,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"web": {CPURequest: &cpu, MemoryRequest: &mem, RemoveCPULimit: true},
			},
			Trace: map[string]sustainv1alpha1.ContainerTrace{
				"web": {CPU: &sustainv1alpha1.ResourceTrace{Percentile: &percentile, WithHeadroom: cpu, Clamped: cpu, RemoveLimit: true}},
			},
		},
	}
	srv := identityServer(t, memHealthSignals{}, deploymentPolicy("p"), deploymentOptingInto("p", "prod", "web", "", time.Now()), stored)

	var got workloadDetailResponse
	detail(t, srv, "prod", "Deployment", "web", &got)

	if !got.Automated || got.PolicyName != "p" || got.UpdateMode != string(sustainv1alpha1.UpdateModeOngoing) {
		t.Errorf("automated/policy/mode = %v/%q/%q, want true/p/Ongoing", got.Automated, got.PolicyName, got.UpdateMode)
	}
	rec := got.Recommendation
	if rec == nil {
		t.Fatal("no stored recommendation in the detail")
	}
	if rec.Outcome != sustainv1alpha1.OutcomeNoData || rec.ObservedAt != "2026-10-01T12:00:00Z" {
		t.Errorf("outcome/observedAt = %q/%q, want NoData/2026-10-01T12:00:00Z", rec.Outcome, rec.ObservedAt)
	}
	if c := rec.Containers["web"]; c.CPURequest != "250m" || c.MemoryRequest != "128Mi" || !c.CPULimitRemoved {
		t.Errorf("web = %+v, want the stored 250m/128Mi with the CPU limit removed", c)
	}
	if tr := rec.Trace["web"].CPU; tr == nil || tr.Percentile.Cmp(percentile) != 0 || tr.Clamped.Cmp(cpu) != 0 || !tr.RemoveLimit {
		t.Errorf("web cpu trace = %+v, want the stored one", tr)
	}
}

// Coordination factors are read from the trace of the stored Recommendation,
// on list rows and the detail page alike; no Prometheus series carries them.
func TestCoordinationFactorsComeFromTheStoredTrace(t *testing.T) {
	cpu, mem := resource.MustParse("300m"), resource.MustParse("176Mi")
	replica := 0.9
	stored := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: wlrcache.Name("Deployment", "api")},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "prod", Name: "api"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Outcome:    sustainv1alpha1.OutcomeComputed,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"api": {CPURequest: &cpu, MemoryRequest: &mem}},
			Trace: map[string]sustainv1alpha1.ContainerTrace{"api": {
				CPU: &sustainv1alpha1.ResourceTrace{WithHeadroom: cpu, Clamped: cpu, Coordination: &sustainv1alpha1.CoordinationTrace{
					OverheadFactor: 1.2, ReplicaFactor: &replica, Scaled: cpu, Value: cpu,
				}},
				Memory: &sustainv1alpha1.ResourceTrace{WithHeadroom: mem, Clamped: mem, Coordination: &sustainv1alpha1.CoordinationTrace{
					OverheadFactor: 1.1, Scaled: mem, Value: mem,
				}},
			}},
		},
	}
	srv := identityServer(t, memHealthSignals{identity("prod", "Deployment", "api"): {AutoscalerPresent: true}},
		deploymentPolicy("p"), deploymentOptingInto("p", "prod", "api", "", time.Now()),
		deploymentOptingInto("p", "prod", "web", "", time.Now()), stored)
	want := coordinationFactors{Enabled: true, CPUOverhead: 1.2, MemoryOverhead: 1.1, CPUReplica: 0.9}

	rec := httptest.NewRecorder()
	srv.handleAllWorkloads(rec, httptest.NewRequest(http.MethodGet, "/api/workloads", nil))
	var page struct {
		Items []struct {
			Name                string               `json:"name"`
			AutoscalerPresent   bool                 `json:"autoscalerPresent"`
			CoordinationFactors *coordinationFactors `json:"coordinationFactors"`
		} `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &page)
	if len(page.Items) != 2 {
		t.Fatalf("rows = %+v, want api and web", page.Items)
	}
	for _, row := range page.Items {
		switch row.Name {
		case "api":
			if !row.AutoscalerPresent || row.CoordinationFactors == nil || *row.CoordinationFactors != want {
				t.Errorf("api row = %v %+v, want the autoscaler and the traced factors %+v", row.AutoscalerPresent, row.CoordinationFactors, want)
			}
		case "web":
			if row.CoordinationFactors != nil {
				t.Errorf("web has no stored trace, got factors %+v", row.CoordinationFactors)
			}
		}
	}

	var got workloadDetailResponse
	detail(t, srv, "prod", "Deployment", "api", &got)
	if got.CoordinationFactors == nil || *got.CoordinationFactors != want {
		t.Errorf("detail factors = %+v, want %+v", got.CoordinationFactors, want)
	}
}

func TestWorkloadDetailOfUnknownIdentityIs404(t *testing.T) {
	srv := identityServer(t, memHealthSignals{})
	rec := httptest.NewRecorder()
	srv.handleWorkloadDetail(rec, httptest.NewRequest(http.MethodGet, "/api/workloads/prod/Deployment/ghost", nil), "prod", "Deployment", "ghost")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func sustainEvent(name, kind, object, source string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "prod", Name: name},
		InvolvedObject: corev1.ObjectReference{Namespace: "prod", Kind: kind, Name: object},
		Reason:         "ResourcesUpdated",
		Source:         corev1.EventSource{Component: source},
		LastTimestamp:  metav1.NewTime(at),
	}
}

// The controller attributes events to the member it acted on, so an
// owner-name identity's events are read from every member, and only theirs.
func TestWorkloadDetailReadsEventsOfEveryMember(t *testing.T) {
	now := time.Now()
	objs := []client.Object{
		deploymentPolicy("p"),
		deploymentWithOwnerName("prod", "api-blue", "api", now),
		deploymentWithOwnerName("prod", "api-green", "api", now),
		sustainEvent("blue", "Deployment", "api-blue", "k8s-sustain", now.Add(-2*time.Minute)),
		sustainEvent("green", "Deployment", "api-green", "k8s-sustain", now.Add(-time.Minute)),
		sustainEvent("other-identity", "Deployment", "web", "k8s-sustain", now),
		sustainEvent("other-source", "Deployment", "api-blue", "deployment-controller", now),
	}
	field := func(name string, value func(*corev1.Event) string) func(*fake.ClientBuilder) *fake.ClientBuilder {
		return func(b *fake.ClientBuilder) *fake.ClientBuilder {
			return b.WithIndex(&corev1.Event{}, name, func(o client.Object) []string { return []string{value(o.(*corev1.Event))} })
		}
	}
	b := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...)
	for _, index := range []func(*fake.ClientBuilder) *fake.ClientBuilder{
		field("source", func(e *corev1.Event) string { return e.Source.Component }),
		field("involvedObject.kind", func(e *corev1.Event) string { return e.InvolvedObject.Kind }),
		field("involvedObject.name", func(e *corev1.Event) string { return e.InvolvedObject.Name }),
	} {
		b = index(b)
	}
	srv := &Server{K8sClient: b.Build(), Logger: testLogger(t), PromClient: &fakePromClient{}, Health: memHealthSignals{}}

	var got workloadDetailResponse
	detail(t, srv, "prod", "Deployment", "api", &got)

	var names []string
	for _, e := range got.RecentEvents {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"api-green", "api-blue"}) {
		t.Errorf("events about %v, want both members' k8s-sustain events, newest first", names)
	}
}
