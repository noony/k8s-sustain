package wlrcache_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
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
