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
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/inventory"
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

// basis is what the tests' snapshots are taken with: dated at base.
func basis() inventory.Basis {
	return inventory.Basis{Clock: clocktesting.NewFakePassiveClock(base)}
}

// take snapshots objs on opts with basis's clock.
func take(t *testing.T, opts inventory.Options, objs ...client.Object) *inventory.Snapshot {
	t.Helper()
	opts.Clock = basis().Clock
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

// A member is decided on its pod template, its object's own annotations and
// its Namespace's; the Policy's label selector matches the pod template's
// labels, never the object's own.
func TestMembersAreGovernedThroughTheirPodTemplate(t *testing.T) {
	selective := managing("selective", sustainv1alpha1.UpdateTypes{Deployment: ongoing()})
	selective.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "web"}}
	templateLabelled := deployment("prod", "template-labelled", base, optIn("selective"))
	templateLabelled.Spec.Template.Labels = map[string]string{"tier": "web"}
	objectLabelled := deployment("prod", "object-labelled", base, optIn("selective"))
	objectLabelled.Labels = map[string]string{"tier": "web"}
	workloadLevel := deployment("prod", "workload-level", base, nil)
	workloadLevel.Annotations = optIn("p")
	team := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", Annotations: optIn("p")}}

	snap := take(t, inventory.Options{Basis: inventory.Basis{ExcludedNamespaces: []string{"kube-system"}}},
		everyKind("p"), selective, team, templateLabelled, objectLabelled, workloadLevel,
		deployment("team", "namespace-level", base, nil),
		deployment("kube-system", "excluded", base, optIn("p")),
	)

	for k, want := range map[promclient.WorkloadIdentity]string{
		key("prod", "Deployment", "template-labelled"): "selective",
		key("prod", "Deployment", "object-labelled"):   "",
		key("prod", "Deployment", "workload-level"):    "p",
		key("team", "Deployment", "namespace-level"):   "p",
		key("kube-system", "Deployment", "excluded"):   "",
	} {
		id := lookup(t, snap, k)
		if id.Policy != want || id.Members[0].Policy != want {
			t.Errorf("%s: policy = %q, want %q", k.OwnerName, id.Policy, want)
		}
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

// A snapshot decides with the lister it is handed, so a caller holding one
// lister compiles a version of a Policy once, not once per snapshot. The same
// version listed with other content tells the two apart.
func TestTakeDecidesWithTheListerItIsHanded(t *testing.T) {
	objs := func(tier string) []client.Object {
		p := everyKind("p")
		p.UID, p.ResourceVersion = "uid-p", "7"
		p.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": tier}}
		web := deployment("a", "web", base, optIn("p"))
		web.Spec.Template.Labels = map[string]string{"tier": "web"}
		return []client.Object{p, web}
	}
	policyOfWeb := func(opts inventory.Options, tier string) string {
		return lookup(t, take(t, opts, objs(tier)...), key("a", "Deployment", "web")).Policy
	}
	lister := &governance.PolicyLister{}

	if got := policyOfWeb(inventory.Options{Basis: inventory.Basis{Policies: lister}}, "web"); got != "p" {
		t.Fatalf("first snapshot: policy = %q, want p", got)
	}
	if got := policyOfWeb(inventory.Options{Basis: inventory.Basis{Policies: lister}}, "api"); got != "p" {
		t.Errorf("with the lister that compiled this version: policy = %q, want p", got)
	}
	if got := policyOfWeb(inventory.Options{}, "api"); got != "" {
		t.Errorf("with a lister of its own: policy = %q, want none", got)
	}
}

// A Policy's snapshot reads only its selector's namespaces and the kinds it
// manages, governs nothing in an excluded namespace, and is empty for a
// Policy managing no kind rather than reading every kind. Either is dated by
// its clock: a sweep keeps what was created after it.
func TestTakeForPolicyReadsWhatThePolicyCanGovern(t *testing.T) {
	p := managing("p", sustainv1alpha1.UpdateTypes{Deployment: ongoing()})
	p.Spec.Selector.Namespaces = []string{"a", "excluded"}
	none := managing("none", sustainv1alpha1.UpdateTypes{})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		p, none,
		deployment("a", "web", base, optIn("p")),
		deployment("b", "web", base, optIn("p")),
		deployment("excluded", "web", base, optIn("p")),
		job("a", "nightly", optIn("p")),
		deployment("a", "idle", base, optIn("none")),
	).Build()

	takenAt := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	b := inventory.Basis{ExcludedNamespaces: []string{"excluded"}, Clock: clocktesting.NewFakePassiveClock(takenAt)}
	snap, err := inventory.TakeForPolicy(context.Background(), c, p, b)
	if err != nil {
		t.Fatalf("TakeForPolicy: %v", err)
	}
	if !snap.TakenAt.Equal(takenAt) {
		t.Errorf("TakenAt = %v, want the clock's %v", snap.TakenAt, takenAt)
	}
	var governed []promclient.WorkloadIdentity
	for _, id := range snap.GovernedBy("p") {
		governed = append(governed, id.Key)
	}
	if want := []promclient.WorkloadIdentity{key("a", "Deployment", "web")}; !slices.Equal(governed, want) {
		t.Errorf("governed = %v, want %v", governed, want)
	}
	for _, k := range []promclient.WorkloadIdentity{key("b", "Deployment", "web"), key("a", "Job", "nightly")} {
		if _, ok := snap.Lookup(k); ok {
			t.Errorf("%v read outside the Policy's namespaces and kinds", k)
		}
	}

	snap, err = inventory.TakeForPolicy(context.Background(), c, none, b)
	if err != nil {
		t.Fatalf("TakeForPolicy: %v", err)
	}
	if len(snap.Identities) != 0 {
		t.Errorf("a Policy managing no kind read %v", keys(snap))
	}
	if !snap.TakenAt.Equal(takenAt) {
		t.Errorf("TakenAt of the empty snapshot = %v, want the clock's %v", snap.TakenAt, takenAt)
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
	snap, err := inventory.Take(context.Background(), listErr(isRollouts, noCRD), inventory.Options{Basis: basis()})
	if err != nil {
		t.Fatalf("Take with no Rollout CRD: %v", err)
	}
	if _, ok := snap.Lookup(key("a", "Deployment", "web")); !ok {
		t.Error("the other kinds must still be listed")
	}

	if _, err := inventory.Take(context.Background(), listErr(isRollouts, errors.New("forbidden")), inventory.Options{Basis: basis()}); err == nil {
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

	for _, opts := range []inventory.Options{{Basis: basis()}, {Basis: basis(), Namespaces: []string{"a"}}} {
		if _, err := inventory.Take(context.Background(), c, opts); err != nil {
			t.Fatalf("Take: %v", err)
		}
	}
	if reads != 0 {
		t.Errorf("Namespace reads = %d, want none for a template-level opt-in", reads)
	}
}

// A snapshot is dated by its reader's clock, never a fallback of its own: a
// Basis without one fails, even for a Policy that reads nothing.
func TestABasisWithoutAClockFails(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(everyKind("p")).Build()
	if _, err := inventory.Take(context.Background(), c, inventory.Options{}); err == nil {
		t.Error("Take without a clock succeeded")
	}
	none := managing("none", sustainv1alpha1.UpdateTypes{})
	if _, err := inventory.TakeForPolicy(context.Background(), c, none, inventory.Basis{}); err == nil {
		t.Error("TakeForPolicy without a clock succeeded")
	}
}
