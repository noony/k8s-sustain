package inventory_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	"github.com/noony/k8s-sustain/internal/policymatch/policymatchtest"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, rolloutsv1alpha1.AddToScheme, sustainv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

func take(t *testing.T, opts inventory.Options, objs ...client.Object) *inventory.Snapshot {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	snap, err := inventory.Take(context.Background(), c, opts)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	return snap
}

func key(ns, kind, name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: ns, OwnerKind: kind, OwnerName: name}
}

func lookup(t *testing.T, snap *inventory.Snapshot, k promclient.WorkloadIdentity) *inventory.Identity {
	t.Helper()
	id, ok := snap.Lookup(k)
	if !ok {
		t.Fatalf("identity %v missing; have %v", k, keys(snap))
	}
	return id
}

func keys(snap *inventory.Snapshot) []promclient.WorkloadIdentity {
	out := make([]promclient.WorkloadIdentity, 0, len(snap.Identities))
	for _, id := range snap.Identities {
		out = append(out, id.Key)
	}
	return out
}

func memberNames(id *inventory.Identity) []string {
	var out []string
	for _, m := range id.Members {
		out = append(out, m.Object.GetName())
	}
	return out
}

func containerNames(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func managing(name string, kinds sustainv1alpha1.UpdateTypes) *sustainv1alpha1.Policy {
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       sustainv1alpha1.PolicySpec{RightSizing: sustainv1alpha1.RightSizingSpec{Update: sustainv1alpha1.UpdateSpec{Types: kinds}}},
	}
}

func ongoing() *sustainv1alpha1.UpdateMode {
	m := sustainv1alpha1.UpdateModeOngoing
	return &m
}

func everyKind(name string) *sustainv1alpha1.Policy {
	return managing(name, sustainv1alpha1.UpdateTypes{
		Deployment: ongoing(), StatefulSet: ongoing(), DaemonSet: ongoing(), ArgoRollout: ongoing(),
		CronJob: ongoing(), Job: ongoing(), Pod: ongoing(),
	})
}

func optIn(policy string) map[string]string {
	return map[string]string{sustainv1alpha1.PolicyAnnotation: policy}
}

func grouped(policy, owner string) map[string]string {
	a := map[string]string{sustainv1alpha1.OwnerNameAnnotation: owner}
	if policy != "" {
		a[sustainv1alpha1.PolicyAnnotation] = policy
	}
	return a
}

func container(name, cpu string) corev1.Container {
	c := corev1.Container{Name: name}
	if cpu != "" {
		c.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
	}
	return c
}

func deployment(ns, name string, created time.Time, annotations map[string]string, containers ...corev1.Container) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)}}
	d.Spec.Template.Annotations = annotations
	d.Spec.Template.Spec.Containers = containers
	return d
}

func job(ns, name string, annotations map[string]string, conditions ...batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(base)}}
	j.Spec.Template.Annotations = annotations
	j.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	for _, c := range conditions {
		j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: c, Status: corev1.ConditionTrue})
	}
	return j
}

func barePod(ns, name string, created time.Time, annotations map[string]string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created), Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func recommendation(ns, kind, name, policy string, created time.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: kind + "-" + name, CreationTimestamp: metav1.NewTime(created)},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: ns, Name: name},
			Policy:      policy,
		},
	}
}

// Members grouped under one owner-name share one identity, whose containers
// are the union of theirs; a container both declare is the newest member's.
func TestOwnerNameGroupUnionsMemberContainers(t *testing.T) {
	blue := deployment("prod", "api-blue", base, grouped("p", "api"), container("app", "100m"), container("sidecar", ""))
	green := deployment("prod", "api-green", base.Add(time.Hour), grouped("p", "api"), container("app", "200m"), container("proxy", ""))
	snap := take(t, inventory.Options{}, everyKind("p"), blue, green)

	if len(snap.Identities) != 1 {
		t.Fatalf("identities = %v, want one owner-name group", keys(snap))
	}
	id := lookup(t, snap, key("prod", "Deployment", "api"))
	if got := memberNames(id); !slices.Equal(got, []string{"api-blue", "api-green"}) {
		t.Errorf("members = %v", got)
	}
	if got := containerNames(id.Containers); !slices.Equal(got, []string{"app", "proxy", "sidecar"}) {
		t.Errorf("containers = %v, want the union", got)
	}
	if cpu := id.Containers[0].Resources.Requests.Cpu().String(); cpu != "200m" {
		t.Errorf("app CPU = %s, want the newest member's 200m", cpu)
	}
	if id.Policy != "p" || id.Conflicted || id.Departed() {
		t.Errorf("policy/conflicted/departed = %q/%v/%v, want p/false/false", id.Policy, id.Conflicted, id.Departed())
	}
}

// An identity's age runs from its earliest member or from when its
// WorkloadRecommendation was created, whichever is older.
func TestSinceIsTheEarliestMemberOrRecommendation(t *testing.T) {
	older := deployment("prod", "api-a", base.Add(2*time.Hour), grouped("p", "api"), container("app", ""))
	newer := deployment("prod", "api-b", base.Add(3*time.Hour), grouped("p", "api"), container("app", ""))

	snap := take(t, inventory.Options{}, everyKind("p"), older, newer)
	if got := lookup(t, snap, key("prod", "Deployment", "api")).Since; !got.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("Since = %v, want the earliest member's creation", got)
	}

	seen := recommendation("prod", "Deployment", "api", "p", base)
	snap = take(t, inventory.Options{}, everyKind("p"), older, newer, seen)
	id := lookup(t, snap, key("prod", "Deployment", "api"))
	if !id.Since.Equal(base) {
		t.Errorf("Since = %v, want the older WorkloadRecommendation's creation", id.Since)
	}
	if id.Recommendation == nil || id.Recommendation.Name != seen.Name {
		t.Errorf("Recommendation = %v, want the stored object", id.Recommendation)
	}
}

// A finished standalone Job is not a live member: its identity is Departed,
// governed by its WorkloadRecommendation's Policy and described by the stored
// snapshot.
func TestFinishedJobIsDeparted(t *testing.T) {
	done := job("batch", "nightly", optIn("p"), batchv1.JobComplete)
	failed := job("batch", "weekly", optIn("p"), batchv1.JobFailed)
	running := job("batch", "hourly", optIn("p"))
	stored := recommendation("batch", "Job", "nightly", "p", base)
	stored.Status.ObservedResources = map[string]sustainv1alpha1.ObservedContainerResources{"app": {CPURequest: resource.NewMilliQuantity(50, resource.DecimalSI)}}

	snap := take(t, inventory.Options{}, everyKind("p"), done, failed, running, stored)

	id := lookup(t, snap, key("batch", "Job", "nightly"))
	if !id.Departed() || id.Policy != "p" {
		t.Errorf("finished Job: departed/policy = %v/%q, want true/p", id.Departed(), id.Policy)
	}
	if got := containerNames(id.Containers); !slices.Equal(got, []string{"app"}) {
		t.Errorf("departed containers = %v, want the stored snapshot's", got)
	}
	if _, ok := snap.Lookup(key("batch", "Job", "weekly")); ok {
		t.Error("a failed Job with no WorkloadRecommendation has no identity")
	}
	if live := lookup(t, snap, key("batch", "Job", "hourly")); live.Departed() {
		t.Error("a running Job is a live member")
	}
}

// A CronJob's Jobs belong to the CronJob identity, never to their own.
func TestCronJobOwnedJobsAreNotIdentities(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "report", UID: "cj-uid"}}
	cj.Spec.JobTemplate.Spec.Template.Annotations = optIn("p")
	cj.Spec.JobTemplate.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	owned := job("batch", "report-123", optIn("p"))
	owned.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: "report", UID: "cj-uid", Controller: new(true)}}

	snap := take(t, inventory.Options{}, everyKind("p"), cj, owned)

	if got := keys(snap); !slices.Equal(got, []promclient.WorkloadIdentity{key("batch", "CronJob", "report")}) {
		t.Errorf("identities = %v, want only the CronJob", got)
	}
}

// Bare pods group by namespace and owner-name; a pod with a controller or
// without a valid owner-name is no bare-pod member.
func TestBarePodsGroupByNamespaceAndOwnerName(t *testing.T) {
	run1 := barePod("airflow", "etl-run-1", base, grouped("p", "etl"), container("old", ""))
	run2 := barePod("airflow", "etl-run-2", base.Add(time.Hour), grouped("p", "etl"), container("new", ""))
	staging := barePod("airflow-staging", "etl-run-1", base, grouped("p", "etl"), container("app", ""))
	anonymous := barePod("airflow", "debug", base, optIn("p"), container("app", ""))
	owned := barePod("airflow", "etl-job-pod", base, grouped("p", "etl"), container("app", ""))
	owned.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "etl-job", Controller: new(true)}}

	snap := take(t, inventory.Options{}, everyKind("p"), run1, run2, staging, anonymous, owned)

	want := []promclient.WorkloadIdentity{key("airflow", "Pod", "etl"), key("airflow-staging", "Pod", "etl")}
	if got := keys(snap); !slices.Equal(got, want) {
		t.Fatalf("identities = %v, want %v", got, want)
	}
	id := lookup(t, snap, key("airflow", "Pod", "etl"))
	if got := memberNames(id); !slices.Equal(got, []string{"etl-run-1", "etl-run-2"}) {
		t.Errorf("members = %v", got)
	}
	if got := containerNames(id.Containers); !slices.Equal(got, []string{"new", "old"}) {
		t.Errorf("containers = %v, want the union, newest first", got)
	}
	if !id.Since.Equal(base) {
		t.Errorf("Since = %v, want the earliest pod", id.Since)
	}
}

// Members governed by different Policies make the identity Conflicted: no
// Policy governs it. An opted-out member is no party to the conflict.
func TestMembersOfDifferentPoliciesAreConflicted(t *testing.T) {
	a := barePod("airflow", "etl-1", base, grouped("p", "etl"), container("app", ""))
	b := barePod("airflow", "etl-2", base, grouped("q", "etl"), container("app", ""))
	optedOut := barePod("airflow", "etl-3", base, map[string]string{
		sustainv1alpha1.OwnerNameAnnotation: "etl", sustainv1alpha1.OptOutAnnotation: "true",
	}, container("app", ""))
	agreeing := deployment("prod", "web", base, optIn("p"), container("app", ""))
	optedOutSibling := deployment("prod", "web-canary", base, map[string]string{
		sustainv1alpha1.OwnerNameAnnotation: "web", sustainv1alpha1.OptOutAnnotation: "true",
	}, container("app", ""))

	snap := take(t, inventory.Options{}, everyKind("p"), everyKind("q"), a, b, optedOut, agreeing, optedOutSibling)

	id := lookup(t, snap, key("airflow", "Pod", "etl"))
	if !id.Conflicted || id.Policy != "" {
		t.Errorf("conflicted/policy = %v/%q, want true and no governing Policy", id.Conflicted, id.Policy)
	}
	if got := id.MemberPolicies(); !slices.Equal(got, []string{"p", "q"}) {
		t.Errorf("member policies = %v, want [p q]", got)
	}
	web := lookup(t, snap, key("prod", "Deployment", "web"))
	if web.Conflicted || web.Policy != "p" {
		t.Errorf("web: conflicted/policy = %v/%q, want an opted-out sibling to leave p governing", web.Conflicted, web.Policy)
	}
	if governed := snap.GovernedBy("p"); len(governed) != 1 || governed[0].Key != web.Key {
		t.Errorf("GovernedBy(p) = %v, want only web", governed)
	}
}

// A member governs nothing through a Policy that does not accept it: missing,
// not managing its kind, or not selecting it.
func TestOptInNeedsThePolicysConsent(t *testing.T) {
	selective := managing("selective", sustainv1alpha1.UpdateTypes{Deployment: ongoing()})
	selective.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "web"}}
	elsewhere := managing("elsewhere", sustainv1alpha1.UpdateTypes{Deployment: ongoing()})
	elsewhere.Spec.Selector.Namespaces = []string{"other"}
	jobsOnly := managing("jobs-only", sustainv1alpha1.UpdateTypes{Job: ongoing()})

	labelled := deployment("prod", "labelled", base, optIn("selective"))
	labelled.Labels = map[string]string{"tier": "web"}
	cases := map[string]*appsv1.Deployment{
		"selector rejects labels": deployment("prod", "unlabelled", base, optIn("selective")),
		"namespace not selected":  deployment("prod", "misplaced", base, optIn("elsewhere")),
		"kind not managed":        deployment("prod", "wrong-kind", base, optIn("jobs-only")),
		"policy missing":          deployment("prod", "dangling", base, optIn("ghost")),
		"excluded namespace":      deployment("kube-system", "excluded", base, optIn("selective")),
	}
	objs := []client.Object{selective, elsewhere, jobsOnly, labelled}
	for _, d := range cases {
		objs = append(objs, d)
	}
	snap := take(t, inventory.Options{ExcludedNamespaces: []string{"kube-system"}}, objs...)

	if got := lookup(t, snap, key("prod", "Deployment", "labelled")).Policy; got != "selective" {
		t.Errorf("labelled: policy = %q, want selective", got)
	}
	for name, d := range cases {
		id := lookup(t, snap, key(d.Namespace, "Deployment", d.Name))
		if id.Policy != "" || id.Members[0].Policy != "" {
			t.Errorf("%s: policy = %q, want none", name, id.Policy)
		}
	}
}

// Replays the shared annotation contract: the three opt-in levels resolve
// here exactly as policymatch.ResolvePolicy says.
func TestAnnotationLevels(t *testing.T) {
	for _, tc := range policymatchtest.AnnotationCases() {
		t.Run(tc.Name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Annotations: tc.Namespace}}
			d := deployment("team-a", "web", base, tc.Template, container("app", ""))
			d.Annotations = tc.Workload

			snap := take(t, inventory.Options{}, everyKind("p"), everyKind("other"), ns, d)

			if got := lookup(t, snap, key("team-a", "Deployment", "web")).Policy; got != tc.WantPolicy {
				t.Errorf("policy = %q, want %q", got, tc.WantPolicy)
			}
		})
	}
}

// Narrowing to namespaces and kinds reads only those, and a Departed
// identity's Policy must still cover its namespace.
func TestOptionsNarrowTheSnapshot(t *testing.T) {
	p := everyKind("p")
	p.Spec.Selector.Namespaces = []string{"a"}
	objs := []client.Object{
		p,
		deployment("a", "web", base, optIn("p")),
		deployment("b", "web", base, optIn("p")),
		job("a", "nightly", optIn("p")),
		recommendation("a", "Deployment", "gone", "p", base),
		recommendation("b", "Deployment", "gone", "p", base),
	}

	snap := take(t, inventory.Options{Namespaces: []string{"a", "a"}, Kinds: []string{"Deployment"}}, objs...)
	want := []promclient.WorkloadIdentity{key("a", "Deployment", "gone"), key("a", "Deployment", "web")}
	if got := keys(snap); !slices.Equal(got, want) {
		t.Errorf("narrowed identities = %v, want %v", got, want)
	}

	snap = take(t, inventory.Options{}, objs...)
	if got := lookup(t, snap, key("a", "Deployment", "gone")).Policy; got != "p" {
		t.Errorf("departed in a selected namespace: policy = %q, want p", got)
	}
	if got := lookup(t, snap, key("b", "Deployment", "gone")).Policy; got != "" {
		t.Errorf("departed outside the selector: policy = %q, want none", got)
	}
}

// A kind the cluster does not serve has no identities; any other read error
// fails the snapshot.
func TestUnservedKindIsEmptyButOtherErrorsFail(t *testing.T) {
	listErr := func(match func(client.ObjectList) bool, err error) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme(t)).
			WithObjects(everyKind("p"), deployment("a", "web", base, optIn("p"))).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if match(list) {
						return err
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
	}
	isRollouts := func(l client.ObjectList) bool { _, ok := l.(*rolloutsv1alpha1.RolloutList); return ok }

	noCRD := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "argoproj.io", Kind: "Rollout"}}
	snap, err := inventory.Take(context.Background(), listErr(isRollouts, noCRD), inventory.Options{})
	if err != nil {
		t.Fatalf("Take with no Rollout CRD: %v", err)
	}
	if _, ok := snap.Lookup(key("a", "Deployment", "web")); !ok {
		t.Error("the other kinds must still be listed")
	}

	if _, err := inventory.Take(context.Background(), listErr(isRollouts, errors.New("forbidden")), inventory.Options{}); err == nil {
		t.Error("a failed read must fail the snapshot")
	}
}

// The Namespace is read only when an object's own annotations leave its
// opt-in undecided.
func TestNamespaceReadOnlyWhenUndecided(t *testing.T) {
	reads := 0
	c := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(everyKind("p"), deployment("a", "web", base, optIn("p"))).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					reads++
				}
				return c.Get(ctx, k, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NamespaceList); ok {
					reads++
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	for _, opts := range []inventory.Options{{}, {Namespaces: []string{"a"}}} {
		if _, err := inventory.Take(context.Background(), c, opts); err != nil {
			t.Fatalf("Take: %v", err)
		}
	}
	if reads != 0 {
		t.Errorf("Namespace reads = %d, want none for a template-level opt-in", reads)
	}
}
