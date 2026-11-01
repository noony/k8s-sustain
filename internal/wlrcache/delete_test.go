package wlrcache_test

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func ref(kind, name string) sustainv1alpha1.WorkloadReference {
	return sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: "ns", Name: name}
}

// object is the WorkloadRecommendation of r under policy, labelled for it,
// created and last computed at the given times, holding a Recommendation for
// "app".
func object(r sustainv1alpha1.WorkloadReference, policy string, created, computedAt time.Time) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.Namespace, Name: wlrcache.Name(r.Kind, r.Name), UID: types.UID(r.Name + "-uid"),
			Labels:            map[string]string{sustainv1alpha1.WLRPolicyLabel: policy},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: r, Policy: policy},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(computedAt),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			ComputedBy: policy,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"app": {CPURequest: qty("200m")}},
		},
	}
}

// stub is the webhook's stub of r under policy, created at created, before
// any pass recorded anything.
func stub(r sustainv1alpha1.WorkloadReference, policy string, created time.Time) *sustainv1alpha1.WorkloadRecommendation {
	w := object(r, policy, created, time.Time{})
	w.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
	return w
}

// standings answers where each identity stands from a table keyed by owner
// name; every other identity is Ungoverned, as one the snapshot does not hold.
func standings(table map[string]wlrcache.Standing) wlrcache.Standings {
	return func(id promclient.WorkloadIdentity) wlrcache.Standing { return table[id.OwnerName] }
}

// The sweep verdict in every standing an identity can have, for the sweep of
// Policy "p" against a snapshot taken at at, with a 72h retention window.
func TestExpiredPerStanding(t *testing.T) {
	at := time.Now()
	old := at.Add(-48 * time.Hour)
	for _, tc := range []struct {
		name     string
		known    *sustainv1alpha1.WorkloadRecommendation
		standing wlrcache.Standing
		want     bool
	}{
		{"another Policy's object", object(web, "q", old, at.Add(-time.Hour)), wlrcache.Ungoverned, false},
		{"created within the grace period before the snapshot", object(web, "p", at.Add(-time.Minute), at), wlrcache.Ungoverned, false},
		{"created after the snapshot", object(web, "p", at.Add(15*time.Minute), at.Add(15*time.Minute)), wlrcache.Ungoverned, false},
		{"a stub created within the grace period, its status not written yet", stub(web, "p", at.Add(-time.Minute)), wlrcache.Ungoverned, false},
		{"governed", object(web, "p", old, at.Add(-time.Hour)), wlrcache.Governed, false},
		{"ungoverned", object(web, "p", old, at.Add(-time.Hour)), wlrcache.Ungoverned, true},
		{"departed within retention", object(web, "p", old, at.Add(-time.Hour)), wlrcache.Departed, false},
		{"departed past retention", object(web, "p", at.Add(-200*time.Hour), at.Add(-80*time.Hour)), wlrcache.Departed, true},
		{"departed, never computed, created within retention", stub(web, "p", old), wlrcache.Departed, false},
		{"departed, never computed, created past retention", stub(web, "p", at.Add(-80*time.Hour)), wlrcache.Departed, true},
		{"Conflicted within retention", object(web, "p", old, at.Add(-time.Hour)), wlrcache.Conflicted, false},
		{"Conflicted past retention", object(web, "p", at.Add(-200*time.Hour), at.Add(-80*time.Hour)), wlrcache.Conflicted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wlrcache.Expired(tc.known, "p", tc.standing, at, lifetimes); got != tc.want {
				t.Errorf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

// Retention has one meaning: the webhook serves a departed or Conflicted
// identity's Recommendation for exactly as long as the sweep keeps its object.
func TestARetainedRecommendationIsServedAsLongAsItsObjectIsKept(t *testing.T) {
	for _, tc := range []struct {
		name     string
		d        wlrcache.Decision
		standing wlrcache.Standing
	}{
		{"departed", wlrcache.Decision{
			Outcome: sustainv1alpha1.OutcomeComputed, Departed: true, Recs: computedDecision("200m").Recs,
		}, wlrcache.Departed},
		{"Conflicted", wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeConflicted}, wlrcache.Conflicted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			computedAt := time.Now().Truncate(time.Second)
			c := newCluster(t, computed(0, computedAt))
			if err := wlrcache.Record(context.Background(), c, c.mustStored(t, web), tc.d, computedAt, lifetimes); err != nil {
				t.Fatalf("Record: %v", err)
			}
			known := c.mustStored(t, web)
			for _, at := range []time.Time{computedAt.Add(lifetimes.Retention - time.Second), computedAt.Add(lifetimes.Retention + time.Second)} {
				served := wlrcache.Read(known, "p", at).Verdict.Injects()
				kept := !wlrcache.Expired(known, "p", tc.standing, at, lifetimes)
				if served != kept {
					t.Errorf("%s after the last computation: served %v, kept %v", at.Sub(computedAt), served, kept)
				}
			}
		})
	}
}

// The sweep judges only the objects labelled for its Policy, each by where its
// identity stands.
func TestSweepDeletesThePolicysExpiredObjects(t *testing.T) {
	at := time.Now()
	old := at.Add(-48 * time.Hour)
	optout, hook, foreign := ref("Deployment", "optout"), ref("Job", "hook"), ref("Deployment", "foreign")
	c := newCluster(t,
		object(web, "p", old, at.Add(-time.Hour)),
		object(optout, "p", old, at.Add(-time.Hour)),
		object(hook, "p", at.Add(-200*time.Hour), at.Add(-80*time.Hour)),
		object(foreign, "q", old, at.Add(-time.Hour)),
	)

	deleted, err := wlrcache.Sweep(context.Background(), c, "p",
		standings(map[string]wlrcache.Standing{"web": wlrcache.Governed, "hook": wlrcache.Departed}), at, lifetimes)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	for _, r := range []sustainv1alpha1.WorkloadReference{web, foreign} {
		if c.stored(t, r) == nil {
			t.Errorf("%s was deleted", r.Name)
		}
	}
	for _, r := range []sustainv1alpha1.WorkloadReference{optout, hook} {
		if c.stored(t, r) != nil {
			t.Errorf("%s survived the sweep", r.Name)
		}
	}
}

// A pass that waits on evictions sweeps long after it took its snapshot. A
// webhook stub created in between is in no snapshot, so it stands Ungoverned,
// and judged by the sweep's own clock it was past the grace period: a
// short-lived bare pod lost its only container snapshot. Judged by the
// snapshot's instant, it is kept.
func TestSweepKeepsAStubCreatedAfterItsSnapshot(t *testing.T) {
	now := time.Now()
	snapshotAt := now.Add(-12 * time.Minute)
	optout := ref("Deployment", "optout")
	late := stub(task, "p", now.Add(-11*time.Minute))
	late.Status.ObservedResources = snapshotOf("100m", "app")
	c := newCluster(t, late, object(optout, "p", snapshotAt.Add(-48*time.Hour), snapshotAt.Add(-time.Hour)))

	deleted, err := wlrcache.Sweep(context.Background(), c, "p", standings(nil), snapshotAt, lifetimes)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if c.stored(t, task) == nil {
		t.Error("a stub created after the snapshot was swept")
	}
	if deleted != 1 || c.stored(t, optout) != nil {
		t.Errorf("deleted = %d, want the opted-out identity's object swept", deleted)
	}
}

func TestReleaseDeletesEveryObjectOfThePolicy(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	foreign := ref("Deployment", "foreign")
	c := newCluster(t, object(web, "p", old, old), stub(task, "p", old), object(foreign, "q", old, old))

	deleted, err := wlrcache.Release(context.Background(), c, "p")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if deleted != 2 || c.stored(t, web) != nil || c.stored(t, task) != nil {
		t.Errorf("deleted = %d, want both of p's objects", deleted)
	}
	if c.stored(t, foreign) == nil {
		t.Error("another Policy's object was released")
	}
}

// The reaper collects the objects of Policies that are gone and nothing else.
// An identity whose passes record NoData is retried every cycle; reaping it
// for its age would turn a self-healing state into a cold start.
func TestReapDeletesOnlyTheObjectsOfAPolicyThatIsGone(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	orphan, untracked, idle := ref("Deployment", "orphan"), ref("Deployment", "untracked"), ref("Job", "idle")
	nodata := stub(idle, "live", time.Now().Add(-1000*time.Hour))
	nodata.Status.Outcome = sustainv1alpha1.OutcomeNoData
	c := newCluster(t,
		&sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "live"}},
		object(web, "live", old, old),
		object(orphan, "ghost", old, old),
		object(untracked, "", old, old),
		nodata,
	)

	deleted, err := wlrcache.Reap(context.Background(), c)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if deleted != 1 || c.stored(t, orphan) != nil {
		t.Errorf("deleted = %d, want the object of the Policy that is gone", deleted)
	}
	for _, r := range []sustainv1alpha1.WorkloadReference{web, untracked, idle} {
		if c.stored(t, r) == nil {
			t.Errorf("%s was reaped", r.Name)
		}
	}
}

// deletionPaths runs each way an object is deleted against a cluster whose
// only object is web's under Policy "p", which every path judges deleted: its
// identity stands Ungoverned, and no Policy exists.
func deletionPaths() []struct {
	name string
	// conflictFails says the path reports a conflict instead of leaving the
	// object to its next run.
	conflictFails bool
	run           func(*cluster) (int, error)
} {
	return []struct {
		name          string
		conflictFails bool
		run           func(*cluster) (int, error)
	}{
		{"sweep", false, func(c *cluster) (int, error) {
			return wlrcache.Sweep(context.Background(), c, "p", standings(nil), time.Now(), lifetimes)
		}},
		{"release", true, func(c *cluster) (int, error) { return wlrcache.Release(context.Background(), c, "p") }},
		{"reap", false, func(c *cluster) (int, error) { return wlrcache.Reap(context.Background(), c) }},
	}
}

// Every path judges a copy read from a cache, so each deletes only the
// revision it judged (ADR 0003): the UID refuses a name reused by another
// object, the resourceVersion one rewritten since, typically adopted by
// another Policy (a GitOps rename creates the new Policy before deleting the
// old). The adopted object survives. The release returns the conflict, so its
// Policy's finalizer stays until a retry, which no longer lists the object.
func TestEveryDeletionDeletesOnlyTheRevisionItJudged(t *testing.T) {
	for _, path := range deletionPaths() {
		t.Run(path.name, func(t *testing.T) {
			stored := object(web, "p", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
			c := newCluster(t, stored)
			var judged string
			c.afterList = func(store client.Client, listed []sustainv1alpha1.WorkloadRecommendation) {
				c.afterList = nil
				judged = listed[0].ResourceVersion
				adopted := listed[0].DeepCopy()
				adopted.Spec.Policy = "p2"
				adopted.Labels[sustainv1alpha1.WLRPolicyLabel] = "p2"
				if err := store.Update(context.Background(), adopted); err != nil {
					t.Fatalf("adopting: %v", err)
				}
			}

			deleted, err := path.run(c)
			if path.conflictFails != apierrors.IsConflict(err) || !path.conflictFails && err != nil {
				t.Errorf("err = %v, want a conflict: %v", err, path.conflictFails)
			}
			if deleted != 0 {
				t.Errorf("deleted = %d, want 0", deleted)
			}
			if got := c.mustStored(t, web); got.Spec.Policy != "p2" {
				t.Errorf("spec.policy = %q, want the adopting p2", got.Spec.Policy)
			}
			if len(c.preconditions) != 1 {
				t.Fatalf("deletes = %d, want 1", len(c.preconditions))
			}
			if p := c.preconditions[0]; p.UID == nil || *p.UID != stored.UID || p.ResourceVersion == nil || *p.ResourceVersion != judged {
				t.Errorf("preconditions = %+v, want UID %s at the judged resourceVersion %s", p, stored.UID, judged)
			}
			if !path.conflictFails {
				return
			}
			if _, err := path.run(c); err != nil {
				t.Errorf("retry: %v", err)
			}
			if got := c.mustStored(t, web); got.Spec.Policy != "p2" {
				t.Errorf("after the retry: spec.policy = %q, want the adopting p2", got.Spec.Policy)
			}
		})
	}
}

// An object another writer deleted between the list and the delete is gone,
// which is what every path wanted.
func TestADeletionCountsAnObjectAlreadyGoneAsDeleted(t *testing.T) {
	for _, path := range deletionPaths() {
		t.Run(path.name, func(t *testing.T) {
			c := newCluster(t, object(web, "p", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)))
			c.afterList = func(store client.Client, listed []sustainv1alpha1.WorkloadRecommendation) {
				c.afterList = nil
				if err := store.Delete(context.Background(), listed[0].DeepCopy()); err != nil {
					t.Fatalf("deleting: %v", err)
				}
			}

			deleted, err := path.run(c)
			if err != nil || deleted != 1 {
				t.Errorf("deleted, err = %d, %v, want 1, nil", deleted, err)
			}
		})
	}
}
