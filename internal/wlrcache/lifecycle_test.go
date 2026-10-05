package wlrcache_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qty(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

// computed is a WorkloadRecommendation of identity Deployment/web, governed by
// and computed under Policy "p" age ago, holding a Recommendation for "app".
func computed(age time.Duration, now time.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: wlrcache.Name("Deployment", "web")},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "ns", Name: "web"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(now.Add(-age)),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			ComputedBy: "p",
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: qty("200m"), MemoryRequest: qty("256Mi")},
			},
		},
	}
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
// "p" admitted now under the default freshness bounds.
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
		{"departed, computed a day ago", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(24*time.Hour, now)
			w.Status.Departed = true
			return w
		}, wlrcache.Retained},
		{"departed past the retention window", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(200*time.Hour, now)
			w.Status.Departed = true
			return w
		}, wlrcache.Stale},
		{"Conflicted, frozen a day ago", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(24*time.Hour, now)
			w.Status.Outcome = sustainv1alpha1.OutcomeConflicted
			return w
		}, wlrcache.Retained},
		{"Conflicted past the retention window", func() *sustainv1alpha1.WorkloadRecommendation {
			w := computed(200*time.Hour, now)
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
			got := wlrcache.Read(tc.known(), "p", now, wlrcache.Freshness{})
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
		return w
	}
}

// Departed is what waives the staleness budget, so an object of the same age
// that is neither departed nor Conflicted must still trip it: a broader waiver
// would serve whatever a stuck controller last wrote.
func TestReadWaivesStalenessOnlyForRetainedRecommendations(t *testing.T) {
	now := time.Now()
	live := computed(24*time.Hour, now)
	if got := wlrcache.Read(live, "p", now, wlrcache.Freshness{}).Verdict; got != wlrcache.Stale {
		t.Errorf("live object a day old: verdict = %s, want stale", got)
	}
	departed := computed(24*time.Hour, now)
	departed.Status.Departed = true
	if got := wlrcache.Read(departed, "p", now, wlrcache.Freshness{}).Verdict; got != wlrcache.Retained {
		t.Errorf("departed object a day old: verdict = %s, want retained", got)
	}
}

// The handler's own bounds replace the defaults.
func TestReadAppliesTheGivenFreshness(t *testing.T) {
	now := time.Now()
	f := wlrcache.Freshness{Staleness: time.Hour, Retention: 24 * time.Hour}
	if got := wlrcache.Read(computed(45*time.Minute, now), "p", now, f).Verdict; got != wlrcache.Fresh {
		t.Errorf("45m old under a 1h staleness: verdict = %s, want fresh", got)
	}
	departed := computed(30*time.Hour, now)
	departed.Status.Departed = true
	if got := wlrcache.Read(departed, "p", now, f).Verdict; got != wlrcache.Stale {
		t.Errorf("departed 30h ago under a 24h retention: verdict = %s, want stale", got)
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
	got := wlrcache.Read(w, "p", now, wlrcache.Freshness{}).Recs["app"]
	if got.CPURequest.Cmp(*qty("200m")) != 0 || got.MemoryRequest.Cmp(*qty("256Mi")) != 0 ||
		got.MemoryLimit.Cmp(*qty("512Mi")) != 0 {
		t.Errorf("recs[app] = %+v, want the stored quantities", got)
	}
	if !got.RemoveCPULimit || got.RemoveMemoryLimit {
		t.Errorf("remove flags = cpu %v memory %v, want true/false as stored", got.RemoveCPULimit, got.RemoveMemoryLimit)
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
	if v := wlrcache.Read(got, "p", time.Now(), wlrcache.Freshness{}).Verdict; v != wlrcache.Undecided {
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
	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", union); err != nil {
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
// the snapshot the webhook took of its first pod. A later run with other
// containers must replace it — or the identity is computed for containers it
// no longer runs, and the new ones get nothing, for as long as it lives.
func TestRequestRefreshesTheSnapshotOfADepartedIdentity(t *testing.T) {
	now := time.Now()
	departed := computed(time.Hour, now)
	departed.Status.ObservedAt = metav1.NewTime(departed.Status.ObservedAt.Truncate(time.Second))
	departed.Status.Departed = true
	departed.Status.ObservedResources = snapshotOf("100m", "app")
	c := newCluster(t, departed)
	known := c.mustStored(t, web)

	newRun := snapshotOf("200m", "app", "sidecar")
	if !wlrcache.ShouldRequest(known, "p", newRun) {
		t.Fatal("ShouldRequest = false for a departed identity whose snapshot the admitted pod no longer matches")
	}
	if err := wlrcache.Request(context.Background(), c, known, web, "p", newRun); err != nil {
		t.Fatalf("Request: %v", err)
	}

	got := c.mustStored(t, web)
	if len(got.Status.ObservedResources) != 2 || got.Status.ObservedResources["app"].CPURequest.String() != "200m" {
		t.Errorf("snapshot = %v, want it replaced by the new run's", got.Status.ObservedResources)
	}
	if cpuOf(t, got, "app") != "200m" || got.Status.Outcome != sustainv1alpha1.OutcomeComputed ||
		!got.Status.Departed || !got.Status.ObservedAt.Equal(&departed.Status.ObservedAt) {
		t.Errorf("status = %+v, want the Recommendation, outcome, departed and observedAt untouched", got.Status)
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
		{"departed, another snapshot", withSnapshot(true, "p", snapshotOf("50m", "app")), true},
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
	known, err := wlrcache.Ensure(context.Background(), c, task, "p", snapshotOf("100m", "main"))
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
	known, err := wlrcache.Ensure(context.Background(), c, web, "p", union)
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
		if _, err := wlrcache.Ensure(context.Background(), c, web, "p", observed); err != nil {
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
// touches nothing a pass decided.
func TestEnsureClearsDepartedAndKeepsTheDecision(t *testing.T) {
	now := time.Now()
	departed := computed(30*time.Minute, now)
	departed.Labels = map[string]string{sustainv1alpha1.WLRPolicyLabel: "p"}
	departed.Status.Departed = true
	c := newCluster(t, departed)
	before := c.mustStored(t, web)

	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", nil); err != nil {
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
		if v := wlrcache.Read(got, "p", now, wlrcache.Freshness{}).Verdict; v != wantP {
			t.Errorf("%s: verdict for a pod of p = %s, want %s", step, v, wantP)
		}
		if v := wlrcache.Read(got, "q", now, wlrcache.Freshness{}).Verdict; v != wantQ {
			t.Errorf("%s: verdict for a pod of q = %s, want %s", step, v, wantQ)
		}
	}

	if _, err := wlrcache.Ensure(context.Background(), c, web, "q", nil); err != nil {
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
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), wlrcache.Decision{Outcome: outcome}, now); err != nil {
			t.Fatalf("Record %s: %v", outcome, err)
		}
		verdicts("q recorded "+string(outcome), wlrcache.Withheld, wlrcache.Withheld)
	}

	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("300m"), now); err != nil {
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
	c := newCluster(t, frozen)
	if v := wlrcache.Read(c.mustStored(t, web), "p", now, wlrcache.Freshness{}).Verdict; v != wlrcache.Retained {
		t.Fatalf("while Conflicted: verdict for p = %s, want retained", v)
	}

	known, err := wlrcache.Ensure(context.Background(), c, web, "q", nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := wlrcache.Record(context.Background(), c, known, wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeFetchFailed}, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if v := wlrcache.Read(c.mustStored(t, web), "q", now, wlrcache.Freshness{}).Verdict; v != wlrcache.Withheld {
		t.Errorf("after the conflict resolved to q: verdict for q = %s, want withheld", v)
	}
}

// Ensure created the object moments ago, so the cache has not seen it, and the
// pass's record must still land: a record that re-read it 404'd and was
// dropped, while apply went on evicting pods whose replacements then started
// on template resources.
func TestRecordStoresIntoTheObjectEnsureReturnedWhileTheCacheLags(t *testing.T) {
	c := newCluster(t)
	known, err := wlrcache.Ensure(context.Background(), c, web, "p", snapshotOf("100m", "app"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	now := time.Now()
	if err := wlrcache.Record(context.Background(), c, known, computedDecision("250m"), now); err != nil {
		t.Fatalf("Record must not depend on reading the object back: %v", err)
	}

	got := c.mustStored(t, web)
	if cpuOf(t, got, "app") != "250m" {
		t.Errorf("stored CPU = %s, want 250m", cpuOf(t, got, "app"))
	}
	if v := wlrcache.Read(got, "p", now, wlrcache.Freshness{}).Verdict; v != wlrcache.Fresh {
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
			Percentile: qty("299500u"), WithHeadroom: req, Clamped: req,
		}}},
	}
	if err := wlrcache.Record(context.Background(), c, known, d, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := c.mustStored(t, web)
	app := got.Status.Containers["app"]
	if app.CPURequest.String() != "300m" || app.MemoryRequest.String() != "128Mi" || !app.RemoveCPULimit || !app.RemoveMemoryLimit {
		t.Errorf("containers[app] = %+v, want 300m/128Mi with both limits removed", app)
	}
	if got.Status.Trace["app"].CPU == nil || got.Status.Trace["app"].CPU.Percentile.String() != "299500u" {
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
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, at); err != nil {
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
// RefreshInterval old: otherwise a stable workload's object ages into the
// staleness budget and the webhook stops serving it.
func TestRecordRefreshesObservedAtOnceItIsRefreshIntervalOld(t *testing.T) {
	c := newCluster(t, computed(time.Hour, time.Now()))
	start := time.Now()
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("250m"), start); err != nil {
		t.Fatalf("Record: %v", err)
	}
	later := start.Add(wlrcache.RefreshInterval + time.Second)
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("250m"), later); err != nil {
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
			Percentile: qty(percentile), WithHeadroom: req, Clamped: req,
		}}}
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, now); err != nil {
			t.Fatalf("Record: %v", err)
		}
		return c.mustStored(t, web)
	}
	percentileOf := func(w *sustainv1alpha1.WorkloadRecommendation) string {
		return w.Status.Trace["app"].CPU.Percentile.String()
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
			if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, time.Now()); err != nil {
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
	if err := wlrcache.Record(context.Background(), c, nil, computedDecision("250m"), time.Now()); err == nil {
		t.Error("Record into no object returned nil")
	}

	gone := computed(time.Minute, time.Now())
	if err := wlrcache.Record(context.Background(), c, gone, computedDecision("250m"), time.Now()); err == nil {
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
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, now); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if v := wlrcache.Read(c.mustStored(t, web), "p", now, wlrcache.Freshness{}).Verdict; v != wlrcache.Retained {
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
		if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), nodata, now); err != nil {
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
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, start); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if v := wlrcache.Read(c.mustStored(t, web), "p", start.Add(24*time.Hour), wlrcache.Freshness{}).Verdict; v != wlrcache.Retained {
		t.Errorf("a day after the last member went: verdict = %s, want retained", v)
	}

	if _, err := wlrcache.Ensure(context.Background(), c, web, "p", nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if c.mustStored(t, web).Status.Departed {
		t.Error("the claim on a live identity left it departed")
	}

	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), departed, start.Add(time.Minute)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), computedDecision("200m"), start.Add(2*time.Minute)); err != nil {
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
	if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), d, now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := c.mustStored(t, web)
	if got.Status.Departed {
		t.Error("a Conflicted identity with live members still carries departed")
	}
	if got.Spec.Policy != "p" || cpuOf(t, got, "app") != "200m" || !got.Status.ObservedAt.Equal(&stored.Status.ObservedAt) {
		t.Errorf("object = %+v, want spec.policy, Recommendation and observedAt frozen", got)
	}
	if v := wlrcache.Read(got, "p", now, wlrcache.Freshness{}).Verdict; v != wlrcache.Retained {
		t.Errorf("verdict = %s, want retained", v)
	}
}

// The sweep verdict in every standing an identity can have, for the sweep of
// Policy "p" with a 72h retention window.
func TestExpiredPerStanding(t *testing.T) {
	now := time.Now()
	const retention = 72 * time.Hour
	object := func(policy string, created, computedAgo time.Duration) *sustainv1alpha1.WorkloadRecommendation {
		w := computed(computedAgo, now)
		w.Spec.Policy = policy
		w.CreationTimestamp = metav1.NewTime(now.Add(-created))
		return w
	}
	statusless := object("p", 48*time.Hour, 0)
	statusless.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
	for _, tc := range []struct {
		name      string
		known     *sustainv1alpha1.WorkloadRecommendation
		standing  wlrcache.Standing
		retention time.Duration
		want      bool
	}{
		{"another Policy's object", object("q", 48*time.Hour, time.Hour), wlrcache.Ungoverned, retention, false},
		{"created within the grace period", object("p", time.Minute, 0), wlrcache.Ungoverned, retention, false},
		{"governed", object("p", 48*time.Hour, time.Hour), wlrcache.Governed, retention, false},
		{"ungoverned", object("p", 48*time.Hour, time.Hour), wlrcache.Ungoverned, retention, true},
		{"departed within retention", object("p", 48*time.Hour, time.Hour), wlrcache.Departed, retention, false},
		{"departed past retention", object("p", 200*time.Hour, 80*time.Hour), wlrcache.Departed, retention, true},
		{"departed, retention disabled", object("p", 48*time.Hour, time.Hour), wlrcache.Departed, 0, true},
		{"departed, never computed, created within retention", statusless, wlrcache.Departed, retention, false},
		{"Conflicted within retention", object("p", 48*time.Hour, time.Hour), wlrcache.Conflicted, retention, false},
		{"Conflicted past retention", object("p", 200*time.Hour, 80*time.Hour), wlrcache.Conflicted, retention, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wlrcache.Expired(tc.known, "p", tc.standing, now, tc.retention); got != tc.want {
				t.Errorf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}
