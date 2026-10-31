package wlrcache_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

// lifetimes are the controller settings every test writes under: a 5m
// reconcile interval, so a 30m staleness budget, and a 72h retention window.
var lifetimes = wlrcache.Lifetimes{ReconcileInterval: 5 * time.Minute, Retention: 72 * time.Hour}

// computed is a WorkloadRecommendation of identity Deployment/web, governed by
// and computed under Policy "p" age ago, holding a Recommendation for "app",
// stamped servable as a live identity's.
func computed(age time.Duration, now time.Time) *sustainv1alpha1.WorkloadRecommendation {
	observedAt := now.Add(-age)
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: wlrcache.Name("Deployment", "web")},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "ns", Name: "web"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt:    metav1.NewTime(observedAt),
			ServableUntil: metav1.NewTime(observedAt.Add(lifetimes.StalenessBudget())),
			Outcome:       sustainv1alpha1.OutcomeComputed,
			ComputedBy:    "p",
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: qty("200m"), MemoryRequest: qty("256Mi")},
			},
		},
	}
}

// servableFor is w stamped servable for d past now, negative for a stamp
// already past.
func servableFor(w *sustainv1alpha1.WorkloadRecommendation, now time.Time, d time.Duration) *sustainv1alpha1.WorkloadRecommendation {
	w.Status.ServableUntil = metav1.NewTime(now.Add(d))
	return w
}

func TestNameIsKindDashName(t *testing.T) {
	if got := wlrcache.Name("Deployment", "web"); got != "deployment-web" {
		t.Errorf("Name(Deployment, web) = %q, want deployment-web", got)
	}
	if got := wlrcache.Name("StatefulSet", "db"); got != "statefulset-db" {
		t.Errorf("Name(StatefulSet, db) = %q, want statefulset-db", got)
	}
}

// Names past the 253-char object-name limit are truncated with a stable hash
// suffix. The literal pins the hash: every writer and reader derives the name
// independently, so a change here strands every stored object.
func TestNameTruncatesLongNamesWithAStableHash(t *testing.T) {
	want := "deployment-" + strings.Repeat("a", 231) + "-55335e7810"
	got := wlrcache.Name("Deployment", strings.Repeat("a", 260))
	if got != want {
		t.Errorf("Name(long) = %q, want %q", got, want)
	}
	if len(got) > 253 {
		t.Errorf("Name(long) length = %d, want <= 253", len(got))
	}
}

// The read verdict in every state an object can be in, for a pod of Policy
// "p" admitted now. Whether a Recommendation is served depends on the
// servableUntil stamped on it alone: the reader holds no setting of its own.
func TestReadVerdictPerState(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		known func() *sustainv1alpha1.WorkloadRecommendation
		want  wlrcache.Verdict
	}{
		{"no object", func() *sustainv1alpha1.WorkloadRecommendation { return nil }, wlrcache.Absent},
		{"no pass decided anything yet", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(0, now)
			w.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
			return w
		}, wlrcache.Undecided},
		{"empty container map, no outcome", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(0, now)
			w.Status = sustainv1alpha1.WorkloadRecommendationStatus{
				ObservedAt: metav1.NewTime(now), Containers: map[string]sustainv1alpha1.ContainerRecommendation{},
			}
			return w
		}, wlrcache.Undecided},
		{"NoData without a Recommendation", withoutRecommendation(now, sustainv1alpha1.OutcomeNoData), wlrcache.NoData},
		{"TooYoung without a Recommendation", withoutRecommendation(now, sustainv1alpha1.OutcomeTooYoung), wlrcache.NoData},
		{"FetchFailed without a Recommendation", withoutRecommendation(now, sustainv1alpha1.OutcomeFetchFailed), wlrcache.NoData},
		{"Conflicted without a Recommendation", withoutRecommendation(now, sustainv1alpha1.OutcomeConflicted), wlrcache.NoData},
		{"another Policy's object", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(time.Minute, now)
			w.Spec.Policy = "q"
			return w
		}, wlrcache.Withheld},
		{"another Policy's undecided stub", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(0, now)
			w.Spec.Policy = "q"
			w.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
			return w
		}, wlrcache.Withheld},
		{"another Policy's Recommendation on an adopted object", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(time.Minute, now)
			w.Status.ComputedBy = "q"
			return w
		}, wlrcache.Withheld},
		{"a Recommendation no Policy is recorded to have computed", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(time.Minute, now)
			w.Status.ComputedBy = ""
			return w
		}, wlrcache.Withheld},
		{"computed five minutes ago", func() *sustainv1alpha1.WorkloadRecommendation { return computed(5*time.Minute, now) }, wlrcache.Fresh},
		{"computed two hours ago", func() *sustainv1alpha1.WorkloadRecommendation { return computed(2*time.Hour, now) }, wlrcache.Stale},
		{"computed a day ago, stamped servable for another hour", func() *sustainv1alpha1.WorkloadRecommendation {
			return servableFor(computed(24*time.Hour, now), now, time.Hour)
		}, wlrcache.Fresh},
		{"servable until this very instant", func() *sustainv1alpha1.WorkloadRecommendation {
			return servableFor(computed(time.Minute, now), now, 0)
		}, wlrcache.Stale},
		{"never stamped", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(time.Minute, now)
			w.Status.ServableUntil = metav1.Time{}
			return w
		}, wlrcache.Stale},
		{"departed, servable for two more days", func() *sustainv1alpha1.WorkloadRecommendation {
			w := servableFor(computed(24*time.Hour, now), now, 48*time.Hour)
			w.Status.Departed = true
			return w
		}, wlrcache.Retained},
		{"departed, past its servableUntil", func() *sustainv1alpha1.WorkloadRecommendation {
			w := servableFor(computed(80*time.Hour, now), now, -8*time.Hour)
			w.Status.Departed = true
			return w
		}, wlrcache.Stale},
		{"Conflicted, servable for two more days", func() *sustainv1alpha1.WorkloadRecommendation {
			w := servableFor(computed(24*time.Hour, now), now, 48*time.Hour)
			w.Status.Outcome = sustainv1alpha1.OutcomeConflicted
			return w
		}, wlrcache.Retained},
		{"Conflicted, past its servableUntil", func() *sustainv1alpha1.WorkloadRecommendation {
			w := servableFor(computed(80*time.Hour, now), now, -8*time.Hour)
			w.Status.Outcome = sustainv1alpha1.OutcomeConflicted
			return w
		}, wlrcache.Stale},
		{"NoData keeping an old Recommendation", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(2*time.Hour, now)
			w.Status.Outcome = sustainv1alpha1.OutcomeNoData
			return w
		}, wlrcache.Stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wlrcache.Read(tc.known(), "p", now)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s", got.Verdict, tc.want)
			}
			if got.Verdict.Injects() != (got.Recs != nil) {
				t.Errorf("verdict %s with recs %v: a Recommendation comes with an injecting verdict and only with one", got.Verdict, got.Recs)
			}
		})
	}
}

// withoutRecommendation is an object whose passes recorded outcome but never a
// Recommendation, its outcome stamped two hours ago: far beyond any staleness
// budget, which is where such an object spends most of its life. Reporting it
// stale would swamp the stale signal operators alert on with healthy traffic.
func withoutRecommendation(now time.Time, outcome sustainv1alpha1.RecommendationOutcome) func() *sustainv1alpha1.WorkloadRecommendation {
	return func() *sustainv1alpha1.WorkloadRecommendation {
		w := computed(2*time.Hour, now)
		w.Status.Outcome = outcome
		w.Status.Containers = nil
		w.Status.ServableUntil = metav1.Time{}
		return w
	}
}

// The NoLimit intent must round-trip through the read, or the webhook leaves
// the template's limit in place when the Policy says to strip it.
func TestReadCarriesTheWholeRecommendation(t *testing.T) {
	now := time.Now()
	w := computed(time.Minute, now)
	w.Status.Containers["app"] = sustainv1alpha1.ContainerRecommendation{
		CPURequest: qty("200m"), MemoryRequest: qty("256Mi"), MemoryLimit: qty("512Mi"),
		RemoveCPULimit: true,
	}
	got := wlrcache.Read(w, "p", now).Recs["app"]
	if got.CPURequest.Cmp(*qty("200m")) != 0 || got.MemoryRequest.Cmp(*qty("256Mi")) != 0 ||
		got.MemoryLimit.Cmp(*qty("512Mi")) != 0 {
		t.Errorf("recs[app] = %+v, want the stored quantities", got)
	}
	if !got.RemoveCPULimit || got.RemoveMemoryLimit {
		t.Errorf("remove flags = cpu %v memory %v, want true/false as stored", got.RemoveCPULimit, got.RemoveMemoryLimit)
	}
}

// What a Policy holds does not age and survives every later outcome, but is
// never another Policy's numbers.
func TestHeldIsOnlyWhatThePolicyComputed(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		known  func() *sustainv1alpha1.WorkloadRecommendation
		policy string
		want   bool
	}{
		{"none stored", func() *sustainv1alpha1.WorkloadRecommendation { return nil }, "p", false},
		{"computed long ago", func() *sustainv1alpha1.WorkloadRecommendation { return computed(24*time.Hour, now) }, "p", true},
		{"kept through a later outcome", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(time.Hour, now)
			w.Status.Outcome = sustainv1alpha1.OutcomeFetchFailed
			return w
		}, "p", true},
		{"computed by another Policy", func() *sustainv1alpha1.WorkloadRecommendation { return computed(time.Minute, now) }, "q", false},
		{"never computed", withoutRecommendation(now, sustainv1alpha1.OutcomeNoData), "p", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wlrcache.Held(tc.known(), tc.policy)
			if tc.want != (got != nil) {
				t.Fatalf("held = %v, want a Recommendation: %v", got, tc.want)
			}
			if tc.want && got["app"].CPURequest.Cmp(*qty("200m")) != 0 {
				t.Errorf("held[app] = %+v, want the stored 200m", got["app"])
			}
		})
	}
}

// Every test below runs against cluster, whose cache lags behind every
// create: each writer is exercised under the lag it meets in production.

var (
	web  = sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "ns", Name: "web"}
	task = sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"}
)

// snapshotOf is an observed-resources snapshot of containers, each requesting
// cpu.
func snapshotOf(cpu string, containers ...string) map[string]sustainv1alpha1.ObservedContainerResources {
	out := make(map[string]sustainv1alpha1.ObservedContainerResources, len(containers))
	for _, name := range containers {
		out[name] = sustainv1alpha1.ObservedContainerResources{CPURequest: qty(cpu)}
	}
	return out
}

func computedDecision(cpu string) wlrcache.Decision {
	return wlrcache.Decision{
		Outcome: sustainv1alpha1.OutcomeComputed,
		Recs:    map[string]workload.ContainerRecommendation{"app": {CPURequest: qty(cpu)}},
	}
}

func cpuOf(t *testing.T, wlr *sustainv1alpha1.WorkloadRecommendation, container string) string {
	t.Helper()
	c, ok := wlr.Status.Containers[container]
	if !ok || c.CPURequest == nil {
		t.Fatalf("no CPU request stored for %s: %+v", container, wlr.Status.Containers)
	}
	return c.CPURequest.String()
}

// The webhook reads an identity's object by Key, and its writers name the
// identity by WorkloadRef: both find the same object.
func TestAnIdentitysObjectIsWrittenAtItsKey(t *testing.T) {
	c := newCluster(t)
	id := promclient.WorkloadIdentity{Namespace: task.Namespace, OwnerKind: task.Kind, OwnerName: task.Name}
	if got := wlrcache.WorkloadRef(id); got != task {
		t.Fatalf("WorkloadRef = %+v, want %+v", got, task)
	}
	if err := wlrcache.Request(context.Background(), c, nil, wlrcache.WorkloadRef(id), "p", snapshotOf("100m", "app")); err != nil {
		t.Fatalf("Request: %v", err)
	}
	var got sustainv1alpha1.WorkloadRecommendation
	if err := c.store.Get(context.Background(), wlrcache.Key(id), &got); err != nil {
		t.Fatalf("reading the object at Key: %v", err)
	}
	if got.Spec.WorkloadRef != task {
		t.Errorf("object at Key names %+v, want %+v", got.Spec.WorkloadRef, task)
	}
}

// The webhook's stub is what gets an identity the controller never catches
// alive into its work-list, so it must carry the container set to compute
// against, including which containers are init containers.
func TestRequestCreatesAStubCarryingThePodsSnapshot(t *testing.T) {
	c := newCluster(t)
	observed := wlrcache.BuildObservedResources(
		[]corev1.Container{{Name: "worker", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		}}},
		[]corev1.Container{{Name: "prep"}})

	if err := wlrcache.Request(context.Background(), c, nil, task, "p", observed); err != nil {
		t.Fatalf("Request must not depend on reading back its own create: %v", err)
	}

	got := c.mustStored(t, task)
	if got.Spec.WorkloadRef != task || got.Spec.Policy != "p" {
		t.Errorf("spec = %+v, want the admitted identity under p", got.Spec)
	}
	if got.Labels[sustainv1alpha1.WLRPolicyLabel] != "p" || got.Labels[sustainv1alpha1.WLRStubLabel] != "true" {
		t.Errorf("labels = %v, want the policy label for the sweep and the stub provenance marker", got.Labels)
	}
	if w := got.Status.ObservedResources["worker"]; w.Init || w.CPURequest == nil || w.CPURequest.String() != "100m" {
		t.Errorf("worker snapshot = %+v, want a 100m regular container", w)
	}
	if !got.Status.ObservedResources["prep"].Init {
		t.Error("prep not marked Init: ExcludeInitContainers cannot be honoured without it")
	}
	if v := wlrcache.Read(got, "p", time.Now()).Verdict; v != wlrcache.Undecided {
		t.Errorf("verdict on a fresh stub = %s, want undecided", v)
	}
}

// Request is called for an object the webhook's cache reads as absent, which
// is also what it reads right after discovery created one: the create loses,
// and the winner's object — the governing Policy's view of every member —
// stays as it is. A burst of such requests stays harmless.
func TestRequestLosingTheCreateRaceLeavesTheWinnersObject(t *testing.T) {
	c := newCluster(t)
	union := snapshotOf("100m", "app", "sidecar")
	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", union, lifetimes); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for range 5 {
		if err := wlrcache.Request(context.Background(), c, nil, web, "p", snapshotOf("100m", "app")); err != nil {
			t.Fatalf("a Request that loses the create race must succeed quietly: %v", err)
		}
	}

	got := c.mustStored(t, web)
	if len(got.Status.ObservedResources) != 2 {
		t.Errorf("snapshot = %v, want the governing Policy's union left alone", got.Status.ObservedResources)
	}
	if _, stub := got.Labels[sustainv1alpha1.WLRStubLabel]; stub {
		t.Error("the stub marker landed on an object the webhook did not create")
	}
	if creates, _ := c.writes(); creates != 1 {
		t.Errorf("creates = %d, want 1", creates)
	}
}

// A short-lived identity the controller never catches alive is computed from
// the snapshot the webhook took of its first pod. A later run with a new
// container must add it — or the new container gets nothing, for as long as
// the identity lives. Containers the snapshot already holds keep their entry.
func TestRequestAddsNewContainersToTheSnapshotOfADepartedIdentity(t *testing.T) {
	now := time.Now()
	departed := computed(time.Hour, now)
	departed.Status.ObservedAt = metav1.NewTime(departed.Status.ObservedAt.Truncate(time.Second))
	departed.Status.Departed = true
	departed.Status.ObservedResources = snapshotOf("100m", "app")
	c := newCluster(t, departed)
	known := c.mustStored(t, web)

	newRun := snapshotOf("200m", "app", "sidecar")
	if !wlrcache.ShouldRequest(known, "p", newRun) {
		t.Fatal("ShouldRequest = false for a departed identity whose snapshot lacks a container the admitted pod runs")
	}
	if err := wlrcache.Request(context.Background(), c, known, web, "p", newRun); err != nil {
		t.Fatalf("Request: %v", err)
	}

	got := c.mustStored(t, web)
	if len(got.Status.ObservedResources) != 2 || got.Status.ObservedResources["app"].CPURequest.String() != "100m" {
		t.Errorf("snapshot = %v, want sidecar added and app's entry kept", got.Status.ObservedResources)
	}
	if cpuOf(t, got, "app") != "200m" || got.Status.Outcome != sustainv1alpha1.OutcomeComputed ||
		!got.Status.Departed || !got.Status.ObservedAt.Equal(&departed.Status.ObservedAt) {
		t.Errorf("status = %+v, want the Recommendation, outcome, departed and observedAt untouched", got.Status)
	}
}

// Pods of one departed group may run different container sets. Once every
// name is in the snapshot, admissions alternating between the sets write
// nothing more.
func TestRequestDoesNotChurnOnAlternatingContainerSets(t *testing.T) {
	now := time.Now()
	departed := computed(time.Hour, now)
	departed.Status.Departed = true
	departed.Status.ObservedResources = snapshotOf("100m", "app")
	c := newCluster(t, departed)

	runs := []map[string]sustainv1alpha1.ObservedContainerResources{
		snapshotOf("100m", "app", "sidecar"),
		snapshotOf("300m", "app"),
		snapshotOf("100m", "app", "sidecar"),
		snapshotOf("300m", "app"),
	}
	for _, run := range runs {
		if err := wlrcache.Request(context.Background(), c, c.mustStored(t, web), web, "p", run); err != nil {
			t.Fatalf("Request: %v", err)
		}
	}
	if _, patches := c.writes(); patches != 1 {
		t.Errorf("status patches = %d, want 1: only the first admission brought a new container", patches)
	}
}

func TestShouldRequest(t *testing.T) {
	now := time.Now()
	withSnapshot := func(departed bool, policy string, snap map[string]sustainv1alpha1.ObservedContainerResources) *sustainv1alpha1.WorkloadRecommendation {
		w := computed(time.Minute, now)
		w.Spec.Policy = policy
		w.Status.Departed = departed
		w.Status.ObservedResources = snap
		return w
	}
	pod := snapshotOf("100m", "app")
	for _, tc := range []struct {
		name  string
		known *sustainv1alpha1.WorkloadRecommendation
		want  bool
	}{
		{"no object", nil, true},
		{"live identity, another snapshot", withSnapshot(false, "p", snapshotOf("100m", "app", "sidecar")), false},
		{"live identity, no snapshot", withSnapshot(false, "p", nil), true},
		{"departed, same snapshot", withSnapshot(true, "p", snapshotOf("100m", "app")), false},
		{"departed, same containers at other values", withSnapshot(true, "p", snapshotOf("50m", "app")), false},
		{"departed, snapshot lacks the pod's container", withSnapshot(true, "p", snapshotOf("50m", "sidecar")), true},
		{"departed, snapshot holds more containers", withSnapshot(true, "p", snapshotOf("50m", "app", "sidecar")), false},
		{"another Policy's departed object", withSnapshot(true, "q", snapshotOf("50m", "app")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wlrcache.ShouldRequest(tc.known, "p", pod); got != tc.want {
				t.Errorf("ShouldRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// The governing Policy's first claim creates the object the pass records
// into. It carries no decision until the pass records one.
func TestEnsureCreatesAnUndecidedObject(t *testing.T) {
	c := newCluster(t)
	known, err := wlrcache.Ensure(context.Background(), c, task, "p", snapshotOf("100m", "main"), lifetimes)
	if err != nil {
		t.Fatalf("Ensure must not depend on reading back its own create: %v", err)
	}

	got := c.mustStored(t, task)
	if got.Spec.Policy != "p" || got.Labels[sustainv1alpha1.WLRPolicyLabel] != "p" {
		t.Errorf("spec.policy/label = %q/%q, want p", got.Spec.Policy, got.Labels[sustainv1alpha1.WLRPolicyLabel])
	}
	if _, ok := got.Status.ObservedResources["main"]; !ok {
		t.Fatalf("snapshot not written: %+v", got.Status)
	}
	if len(got.Status.Containers) != 0 || !got.Status.ObservedAt.IsZero() || got.Status.Outcome != "" {
		t.Errorf("status = %+v, want no decision before a pass records one", got.Status)
	}
	if known.UID != got.UID || known.ResourceVersion != got.ResourceVersion {
		t.Errorf("returned object at %s/%s, stored at %s/%s: the handle must be the object as written",
			known.UID, known.ResourceVersion, got.UID, got.ResourceVersion)
	}
}

// The webhook created a stub the controller's cache has not seen: Ensure's
// create loses, and re-reading the winner from the same cache 404'd, failing
// discovery for an identity that was fine. Ensure claims the object instead,
// and the claim's response is the object.
func TestEnsureClaimsAStubTheCacheHasNotSeen(t *testing.T) {
	c := newCluster(t)
	if err := wlrcache.Request(context.Background(), c, nil, web, "p", snapshotOf("100m", "app")); err != nil {
		t.Fatalf("Request: %v", err)
	}

	union := snapshotOf("100m", "app", "sidecar")
	known, err := wlrcache.Ensure(context.Background(), c, web, "p", union, lifetimes)
	if err != nil {
		t.Fatalf("Ensure after a stub its cache has not seen: %v", err)
	}

	got := c.mustStored(t, web)
	if known.UID != got.UID || known.ResourceVersion != got.ResourceVersion {
		t.Errorf("returned object at %s/%s, stored at %s/%s", known.UID, known.ResourceVersion, got.UID, got.ResourceVersion)
	}
	if len(got.Status.ObservedResources) != 2 {
		t.Errorf("snapshot = %v, want the members' union to replace the one pod's view", got.Status.ObservedResources)
	}
	if got.Labels[sustainv1alpha1.WLRStubLabel] != "true" {
		t.Error("the stub marker is provenance and must survive the claim")
	}
}

// One rule for every snapshot writer: replace when it differs. A stable
// identity costs no write, and a container removed from the members is
// removed from the snapshot, not merged into it.
func TestEnsureReplacesTheSnapshotOnlyWhenItDiffers(t *testing.T) {
	c := newCluster(t)
	ensure := func(observed map[string]sustainv1alpha1.ObservedContainerResources) int {
		t.Helper()
		_, before := c.writes()
		if _, err := wlrcache.Ensure(context.Background(), c, web, "p", observed, lifetimes); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		_, after := c.writes()
		return after - before
	}
	ensure(snapshotOf("100m", "app", "sidecar"))
	c.warm()

	if n := ensure(snapshotOf("100m", "app", "sidecar")); n != 0 {
		t.Errorf("unchanged snapshot cost %d status writes, want 0", n)
	}
	if n := ensure(snapshotOf("100m", "app")); n != 1 {
		t.Errorf("changed snapshot cost %d status writes, want 1", n)
	}
	if got := c.mustStored(t, web).Status.ObservedResources; len(got) != 1 {
		t.Errorf("snapshot = %v, want the sidecar gone", got)
	}
	if n := ensure(nil); n != 0 {
		t.Errorf("no snapshot to write cost %d status writes, want 0", n)
	}
}

// An identity with a live member is not departed; the claim says so and
// touches nothing a pass decided. Its Recommendation is servable for the
// staleness budget again, not the retention window: a live identity whose
// passes stop recording must stop being injected.
func TestEnsureClearsDepartedAndKeepsTheDecision(t *testing.T) {
	now := time.Now()
	departed := computed(2*time.Hour, now)
	departed.Labels = map[string]string{sustainv1alpha1.WLRPolicyLabel: "p"}
	departed.Status.Departed = true
	departed.Status.ServableUntil = metav1.NewTime(departed.Status.ObservedAt.Add(lifetimes.Retention))
	c := newCluster(t, departed)
	before := c.mustStored(t, web)
	if v := wlrcache.Read(before, "p", now).Verdict; v != wlrcache.Retained {
		t.Fatalf("while departed: verdict = %s, want retained", v)
	}

	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", nil, lifetimes); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got := c.mustStored(t, web)
	if got.Status.Departed {
		t.Error("departed must be cleared: the identity has a live member again")
	}
	if cpuOf(t, got, "app") != "200m" || got.Status.Outcome != sustainv1alpha1.OutcomeComputed ||
		!got.Status.ObservedAt.Equal(&before.Status.ObservedAt) {
		t.Errorf("status = %+v, want the Recommendation, outcome and observedAt untouched", got.Status)
	}
	if want := before.Status.ObservedAt.Add(lifetimes.StalenessBudget()); !got.Status.ServableUntil.Time.Equal(want) {
		t.Errorf("servableUntil = %v, want the staleness budget past observedAt, %v", got.Status.ServableUntil, want)
	}
	if v := wlrcache.Read(got, "p", now).Verdict; v != wlrcache.Stale {
		t.Errorf("live again, computed two hours ago: verdict = %s, want stale", v)
	}
}

// Another Policy now governs the identity: the claim adopts the object and
// keeps the numbers the previous Policy computed, but they are served to
// neither Policy's pods (ADR 0003). The new Policy's first passes may record
// anything but a Recommendation — fetch failed, no data, too young — and the
// old numbers were served to its pods as fresh, for up to the staleness budget.
// Only its own Recommendation is.
func TestAnAdoptedRecommendationIsWithheldUntilTheAdoptingPolicyComputesItsOwn(t *testing.T) {
	now := time.Now()
	stored := computed(5*time.Minute, now)
	stored.Labels = map[string]string{sustainv1alpha1.WLRPolicyLabel: "p"}
	c := newCluster(t, stored)
	verdicts := func(step string, wantP, wantQ wlrcache.Verdict) {
		t.Helper()
		got := c.mustStored(t, web)
		if v := wlrcache.Read(got, "p", now).Verdict; v != wantP {
			t.Errorf("%s: verdict for a pod of p = %s, want %s", step, v, wantP)
		}
		if v := wlrcache.Read(got, "q", now).Verdict; v != wantQ {
			t.Errorf("%s: verdict for a pod of q = %s, want %s", step, v, wantQ)
		}
	}

	if _, err := wlrcache.Ensure(context.Background(), c, web, "q", nil, lifetimes); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got := c.mustStored(t, web)
	if got.Spec.Policy != "q" || got.Labels[sustainv1alpha1.WLRPolicyLabel] != "q" {
		t.Errorf("spec.policy/label = %q/%q, want q", got.Spec.Policy, got.Labels[sustainv1alpha1.WLRPolicyLabel])
	}
	if cpuOf(t, got, "app") != "200m" || got.Status.ComputedBy != "p" {
		t.Errorf("status = %+v, want p's Recommendation kept and attributed to p", got.Status)
	}
	verdicts("adopted", wlrcache.Withheld, wlrcache.Withheld)

	for _, outcome := range []sustainv1alpha1.RecommendationOutcome{
		sustainv1alpha1.OutcomeFetchFailed, sustainv1alpha1.OutcomeNoData, sustainv1alpha1.OutcomeTooYoung,
	} {
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), wlrcache.Decision{Outcome: outcome}, now, lifetimes); err != nil {
			t.Fatalf("Record %s: %v", outcome, err)
		}
		verdicts("q recorded "+string(outcome), wlrcache.Withheld, wlrcache.Withheld)
	}

	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("300m"), now, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := c.mustStored(t, web); got.Status.ComputedBy != "q" || cpuOf(t, got, "app") != "300m" {
		t.Errorf("status = %+v, want q's Recommendation attributed to q", got.Status)
	}
	verdicts("q computed", wlrcache.Withheld, wlrcache.Fresh)
}

// The same holds when a conflict resolves to the Policy whose pods were not
// receiving the frozen numbers: they were computed under the other one.
func TestAConflictResolvedToTheOtherPolicyWithholdsTheFrozenRecommendation(t *testing.T) {
	now := time.Now()
	frozen := computed(time.Hour, now)
	frozen.Labels = map[string]string{sustainv1alpha1.WLRPolicyLabel: "p"}
	frozen.Status.Outcome = sustainv1alpha1.OutcomeConflicted
	frozen.Status.ServableUntil = metav1.NewTime(frozen.Status.ObservedAt.Add(lifetimes.Retention))
	c := newCluster(t, frozen)
	if v := wlrcache.Read(c.mustStored(t, web), "p", now).Verdict; v != wlrcache.Retained {
		t.Fatalf("while Conflicted: verdict for p = %s, want retained", v)
	}

	known, err := wlrcache.Ensure(context.Background(), c, web, "q", nil, lifetimes)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := wlrcache.Record(context.Background(), c, known, wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeFetchFailed}, now, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if v := wlrcache.Read(c.mustStored(t, web), "q", now).Verdict; v != wlrcache.Withheld {
		t.Errorf("after the conflict resolved to q: verdict for q = %s, want withheld", v)
	}
}

// Ensure created the object moments ago, so the cache has not seen it, and the
// pass's record must still land: a record that re-read it 404'd and was
// dropped, while apply went on evicting pods whose replacements then started
// on template resources.
func TestRecordStoresIntoTheObjectEnsureReturnedWhileTheCacheLags(t *testing.T) {
	c := newCluster(t)
	known, err := wlrcache.Ensure(context.Background(), c, web, "p", snapshotOf("100m", "app"), lifetimes)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	now := time.Now()
	if err := wlrcache.Record(context.Background(), c, known, computedDecision("250m"), now, lifetimes); err != nil {
		t.Fatalf("Record must not depend on reading the object back: %v", err)
	}

	got := c.mustStored(t, web)
	if cpuOf(t, got, "app") != "250m" {
		t.Errorf("stored CPU = %s, want 250m", cpuOf(t, got, "app"))
	}
	if v := wlrcache.Read(got, "p", now).Verdict; v != wlrcache.Fresh {
		t.Errorf("verdict = %s, want fresh", v)
	}
	if len(got.Status.ObservedResources) != 1 {
		t.Errorf("snapshot = %v, want Ensure's kept", got.Status.ObservedResources)
	}
}

// The NoLimit intent and the trace are stored with the values: a nil limit
// alone cannot tell "leave alone" from "remove".
func TestRecordComputedStoresTheWholeRecommendation(t *testing.T) {
	c := newCluster(t, computed(time.Hour, time.Now()))
	known := c.mustStored(t, web)
	now := time.Now()
	req := resource.MustParse("300m")
	d := wlrcache.Decision{
		Outcome: sustainv1alpha1.OutcomeComputed,
		Recs: map[string]workload.ContainerRecommendation{
			"app": {CPURequest: &req, MemoryRequest: qty("128Mi"), RemoveCPULimit: true, RemoveMemoryLimit: true},
		},
		Traces: map[string]sustainv1alpha1.ContainerTrace{"app": {CPU: &sustainv1alpha1.ResourceTrace{
			Request: []sustainv1alpha1.TraceStage{{Stage: "percentile", Value: qty("299500u")}, {Stage: "clamp", Value: &req}},
			Limit:   []sustainv1alpha1.TraceStage{{Stage: "noLimit", Removed: true}},
		}}},
	}
	if err := wlrcache.Record(context.Background(), c, known, d, now, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := c.mustStored(t, web)
	app := got.Status.Containers["app"]
	if app.CPURequest.String() != "300m" || app.MemoryRequest.String() != "128Mi" || !app.RemoveCPULimit || !app.RemoveMemoryLimit {
		t.Errorf("containers[app] = %+v, want 300m/128Mi with both limits removed", app)
	}
	if tr := got.Status.Trace["app"].CPU; tr == nil || !equality.Semantic.DeepEqual(*tr, *d.Traces["app"].CPU) {
		t.Errorf("trace = %+v, want it stored with the values", got.Status.Trace)
	}
	if got.Status.Outcome != sustainv1alpha1.OutcomeComputed || got.Status.ObservedAt.Time.Before(now.Add(-time.Second)) {
		t.Errorf("outcome/observedAt = %s/%v, want Computed at %v", got.Status.Outcome, got.Status.ObservedAt, now)
	}
}

// Write amplification scales with change, not with workload count: an
// unchanged decision costs no write.
func TestRecordIsANoOpForAnUnchangedDecision(t *testing.T) {
	c := newCluster(t, computed(time.Hour, time.Now()))
	now := time.Now()
	record := func(d wlrcache.Decision, at time.Time) {
		t.Helper()
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, at, lifetimes); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	record(computedDecision("250m"), now)
	first := c.mustStored(t, web).ResourceVersion
	record(computedDecision("250m"), now.Add(time.Minute))
	if rv := c.mustStored(t, web).ResourceVersion; rv != first {
		t.Errorf("an unchanged Recommendation bumped resourceVersion %s -> %s", first, rv)
	}

	nodata := wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeNoData}
	record(nodata, now.Add(2*time.Minute))
	second := c.mustStored(t, web).ResourceVersion
	record(nodata, now.Add(time.Hour))
	if rv := c.mustStored(t, web).ResourceVersion; rv != second {
		t.Errorf("an unchanged outcome bumped resourceVersion %s -> %s", second, rv)
	}
}

// An unchanged Recommendation is still rewritten once its observedAt is
// RefreshInterval old: otherwise a stable workload's object ages past its
// servableUntil and the webhook stops serving it.
func TestRecordRefreshesObservedAtOnceItIsRefreshIntervalOld(t *testing.T) {
	c := newCluster(t, computed(time.Hour, time.Now()))
	start := time.Now()
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("250m"), start, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	later := start.Add(wlrcache.RefreshInterval + time.Second)
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("250m"), later, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := c.mustStored(t, web).Status.ObservedAt; got.Time.Before(later.Add(-time.Second)) {
		t.Errorf("observedAt = %v, want it refreshed to %v", got, later)
	}
}

// The trace moves with every sample (a percentile that rounds to the same
// request) and is written only with the values it explains.
func TestRecordWritesTheTraceOnlyWithTheValues(t *testing.T) {
	c := newCluster(t, computed(time.Hour, time.Now()))
	now := time.Now()
	record := func(request, percentile string) *sustainv1alpha1.WorkloadRecommendation {
		t.Helper()
		req := resource.MustParse(request)
		d := computedDecision(request)
		d.Traces = map[string]sustainv1alpha1.ContainerTrace{"app": {CPU: &sustainv1alpha1.ResourceTrace{
			Request: []sustainv1alpha1.TraceStage{{Stage: "percentile", Value: qty(percentile)}, {Stage: "clamp", Value: &req}},
		}}}
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, now, lifetimes); err != nil {
			t.Fatalf("Record: %v", err)
		}
		return c.mustStored(t, web)
	}
	percentileOf := func(w *sustainv1alpha1.WorkloadRecommendation) string {
		return w.Status.Trace["app"].CPU.Request[0].Value.String()
	}

	first := record("250m", "249500u")
	if got := percentileOf(first); got != "249500u" {
		t.Fatalf("trace percentile = %s, want 249500u", got)
	}
	second := record("250m", "249900u")
	if second.ResourceVersion != first.ResourceVersion {
		t.Error("a trace-only change wrote the status")
	}
	if got := percentileOf(record("300m", "299900u")); got != "299900u" {
		t.Errorf("trace percentile = %s, want the 299900u written with the new values", got)
	}
}

// Every outcome but Computed keeps the last Recommendation and its
// observedAt: a departed identity's samples age out of the window while
// retention still holds its Recommendation, and wiping it, or stamping
// observedAt, would strip it or tell the webhook data still exists behind it.
func TestRecordKeepsTheLastRecommendationForEveryOtherOutcome(t *testing.T) {
	for _, outcome := range []sustainv1alpha1.RecommendationOutcome{
		sustainv1alpha1.OutcomeNoData, sustainv1alpha1.OutcomeTooYoung,
		sustainv1alpha1.OutcomeFetchFailed, sustainv1alpha1.OutcomeConflicted,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			stored := computed(2*time.Hour, time.Now())
			stored.Status.ObservedAt = metav1.NewTime(stored.Status.ObservedAt.Truncate(time.Second))
			c := newCluster(t, stored)

			d := wlrcache.Decision{Outcome: outcome}
			if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, time.Now(), lifetimes); err != nil {
				t.Fatalf("Record: %v", err)
			}

			got := c.mustStored(t, web)
			if got.Status.Outcome != outcome {
				t.Errorf("outcome = %s, want %s", got.Status.Outcome, outcome)
			}
			if cpuOf(t, got, "app") != "200m" {
				t.Error("a good Recommendation must never be wiped")
			}
			if !got.Status.ObservedAt.Equal(&stored.Status.ObservedAt) {
				t.Errorf("observedAt = %v, want it kept at %v", got.Status.ObservedAt, stored.Status.ObservedAt)
			}
		})
	}
}

// The caller acts on a stored decision — apply evicts pods whose replacements
// read it — so a record with nothing to land in must fail, never no-op.
func TestRecordFailsWithoutAnObject(t *testing.T) {
	c := newCluster(t)
	if err := wlrcache.Record(context.Background(), c, nil, computedDecision("250m"), time.Now(), lifetimes); err == nil {
		t.Error("Record into no object returned nil")
	}

	gone := computed(time.Minute, time.Now())
	if err := wlrcache.Record(context.Background(), c, gone, computedDecision("250m"), time.Now(), lifetimes); err == nil {
		t.Error("Record into an object that no longer exists returned nil")
	}
}

// A departed identity still has samples in its window, so every cycle
// recomputes the same Recommendation. Recording it and marking it departed
// were two writes that undid each other — two status writes per cycle,
// bypassing the refresh limit, with the webhook's verdict flapping between
// fresh and retained. One record carries both.
func TestADepartedIdentityCostsAtMostOneStatusWritePerRefreshInterval(t *testing.T) {
	start := time.Now()
	c := newCluster(t, computed(0, start))
	departed := computedDecision("200m")
	departed.Departed = true

	const cycle = 5 * time.Minute
	const cycles = 12
	for i := range cycles {
		now := start.Add(time.Duration(i) * cycle)
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, now, lifetimes); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if v := wlrcache.Read(c.mustStored(t, web), "p", now).Verdict; v != wlrcache.Retained {
			t.Fatalf("cycle %d: verdict = %s, want retained", i, v)
		}
	}
	if _, writes := c.writes(); writes > 1+int(cycles*cycle/wlrcache.RefreshInterval) {
		t.Errorf("%d status writes in an hour of 5-minute cycles, want at most one per %s", writes, wlrcache.RefreshInterval)
	}

	nodata := wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeNoData, Departed: true}
	_, before := c.writes()
	for i := range cycles {
		now := start.Add(time.Duration(cycles+i) * cycle)
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), nodata, now, lifetimes); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if _, after := c.writes(); after-before != 1 {
		t.Errorf("%d status writes for a departed identity whose samples aged out, want 1", after-before)
	}
}

// Departed follows the identity: recorded when its last member goes, cleared
// by the claim or the record of a cycle that sees a member again.
func TestDepartedFollowsTheIdentity(t *testing.T) {
	start := time.Now()
	c := newCluster(t, computed(0, start))
	departed := computedDecision("200m")
	departed.Departed = true
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, start, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if v := wlrcache.Read(c.mustStored(t, web), "p", start.Add(24*time.Hour)).Verdict; v != wlrcache.Retained {
		t.Errorf("a day after the last member went: verdict = %s, want retained", v)
	}

	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", nil, lifetimes); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if c.mustStored(t, web).Status.Departed {
		t.Error("the claim on a live identity left it departed")
	}

	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, start.Add(time.Minute), lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("200m"), start.Add(2*time.Minute), lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if c.mustStored(t, web).Status.Departed {
		t.Error("the record of a live cycle left the identity departed")
	}
}

// A Conflicted identity has live members, so it is never departed: its
// record clears a mark left from before its members came back, and freezes
// everything else as the last governing Policy left it.
func TestRecordingConflictedFreezesTheRecommendationAndClearsDeparted(t *testing.T) {
	now := time.Now()
	stored := computed(time.Hour, now)
	stored.Status.ObservedAt = metav1.NewTime(stored.Status.ObservedAt.Truncate(time.Second))
	stored.Status.Departed = true
	c := newCluster(t, stored)

	d := wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeConflicted}
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, now, lifetimes); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := c.mustStored(t, web)
	if got.Status.Departed {
		t.Error("a Conflicted identity with live members still carries departed")
	}
	if got.Spec.Policy != "p" || cpuOf(t, got, "app") != "200m" || !got.Status.ObservedAt.Equal(&stored.Status.ObservedAt) {
		t.Errorf("object = %+v, want spec.policy, Recommendation and observedAt frozen", got)
	}
	if v := wlrcache.Read(got, "p", now).Verdict; v != wlrcache.Retained {
		t.Errorf("verdict = %s, want retained", v)
	}
}

// Every record stamps until when its Recommendation is served, from what it
// stores: the staleness budget past observedAt for a live identity, the
// retention window for a departed or Conflicted one. Only Computed moves
// observedAt, so no other outcome extends the stamp.
func TestRecordStampsServableUntilByWhereTheIdentityStands(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	computedAt := now.Add(-20 * time.Minute)
	for _, tc := range []struct {
		name string
		d    wlrcache.Decision
		want time.Time
	}{
		{"live, Computed", computedDecision("250m"), now.Add(lifetimes.StalenessBudget())},
		{"live, NoData", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeNoData}, computedAt.Add(lifetimes.StalenessBudget())},
		{"live, TooYoung", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeTooYoung}, computedAt.Add(lifetimes.StalenessBudget())},
		{"live, FetchFailed", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeFetchFailed}, computedAt.Add(lifetimes.StalenessBudget())},
		{"departed, Computed", wlrcache.Decision{
			Outcome: sustainv1alpha1.OutcomeComputed, Departed: true,
			Recs: map[string]workload.ContainerRecommendation{"app": {CPURequest: qty("250m")}},
		}, now.Add(lifetimes.Retention)},
		{"departed, NoData", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeNoData, Departed: true}, computedAt.Add(lifetimes.Retention)},
		{"Conflicted", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeConflicted}, computedAt.Add(lifetimes.Retention)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, computed(now.Sub(computedAt), now))
			if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), tc.d, now, lifetimes); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if got := c.mustStored(t, web).Status.ServableUntil; !got.Time.Equal(tc.want) {
				t.Errorf("servableUntil = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTheStalenessBudgetIsThreeReconcileIntervalsAndAtLeastThirtyMinutes(t *testing.T) {
	for interval, want := range map[time.Duration]time.Duration{
		30 * time.Second: 30 * time.Minute,
		5 * time.Minute:  30 * time.Minute,
		10 * time.Minute: 30 * time.Minute,
		20 * time.Minute: time.Hour,
		6 * time.Hour:    18 * time.Hour,
	} {
		if got := (wlrcache.Lifetimes{ReconcileInterval: interval}).StalenessBudget(); got != want {
			t.Errorf("budget at a %s interval = %s, want %s", interval, got, want)
		}
	}
}

// A stable Recommendation is rewritten only every RefreshInterval, yet must
// stay servable whatever the reconcile interval, even when a pass runs one
// interval late. This is what keeps RefreshInterval well below
// MinStalenessBudget: intervals just under 10m, where three of them start to
// set the budget, are where the two come closest.
func TestAStableRecommendationStaysServableThroughALatePass(t *testing.T) {
	for _, interval := range []time.Duration{
		30 * time.Second, time.Minute, 5 * time.Minute, 400 * time.Second, 9 * time.Minute,
		590 * time.Second, 10 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour,
	} {
		t.Run(interval.String(), func(t *testing.T) {
			l := wlrcache.Lifetimes{ReconcileInterval: interval, Retention: lifetimes.Retention}
			c := newCluster(t)
			if _, err := wlrcache.Ensure(context.Background(), c, web, "p", nil, l); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			c.warm()
			start := time.Now().Truncate(time.Second)
			passes := 3 + int(2*wlrcache.RefreshInterval/interval)
			for i := range passes {
				at := start.Add(time.Duration(i) * interval)
				if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("250m"), at, l); err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
				lateNext := at.Add(2*interval - time.Second)
				if v := wlrcache.Read(c.mustStored(t, web), "p", lateNext).Verdict; v != wlrcache.Fresh {
					t.Fatalf("after pass %d, just before a pass one interval late: verdict = %s, want fresh", i, v)
				}
			}
		})
	}
}
