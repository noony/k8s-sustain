package workload

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestGroupBarePods_GroupsByNamespaceAndOwnerName(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	podA1 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "old-container"}}},
	}
	podA2 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-2", CreationTimestamp: metav1.NewTime(newer),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "new-container"}}},
	}
	// Same owner-name, different namespace — must NOT collapse with podA1/podA2.
	podB := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow-staging", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	groups := GroupBarePods([]corev1.Pod{podA1, podA2, podB})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (one per namespace), got %d: %+v", len(groups), groups)
	}
	for _, g := range groups {
		if g.Name != "etl-daily" {
			t.Errorf("Name = %s, want etl-daily", g.Name)
		}
		if g.Namespace == "airflow" {
			if len(g.Containers) != 1 || g.Containers[0].Name != "new-container" {
				t.Errorf("expected the more recently created pod's container, got %+v", g.Containers)
			}
		}
	}
}

func TestGroupBarePods_NoOwnerNameAnnotation_NotDiscovered(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "standalone",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for a pod with no owner-name annotation, got %d", len(groups))
	}
}

func TestGroupBarePods_OwnedPod_NotDiscovered(t *testing.T) {
	ctrlBool := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "owned",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "etl-daily-job", Controller: &ctrlBool}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for an owned pod (handled by another kind's listing instead), got %d", len(groups))
	}
}

// TestGroupBarePods_CollectsMembers verifies the group carries every pod that
// belongs to it, not just the representative: callers that resize the identity
// in place act on all of them, and they must not re-derive membership
// themselves. A pod carrying a controller ownerRef must never appear as a
// member even when it shares the owner-name annotation — that exclusion is the
// bare-pod analogue of the ownerRef-UID check protecting every other kind.
func TestGroupBarePods_CollectsMembers(t *testing.T) {
	mk := func(name string, controlled bool) corev1.Pod {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "ns",
				Annotations: map[string]string{
					sustainv1alpha1.PolicyAnnotation:    "pol",
					sustainv1alpha1.OwnerNameAnnotation: "dag-task",
				},
			},
		}
		if controlled {
			yes := true
			p.OwnerReferences = []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "u", Controller: &yes},
			}
		}
		return p
	}
	pods := []corev1.Pod{mk("a", false), mk("b", false), mk("impostor", true)}

	groups := GroupBarePods(pods)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if len(groups[0].Members) != 2 {
		t.Fatalf("got %d members, want 2", len(groups[0].Members))
	}
	for _, m := range groups[0].Members {
		if m.Name == "impostor" {
			t.Error("a pod with a controller ownerRef must never be a group member")
		}
	}
}

// TestGroupBarePods_MembersAreNamespaceScoped guards the grouping key: two
// namespaces sharing an owner-name must not pool their pods into one group's
// member list, or a resize would reach across namespaces.
func TestGroupBarePods_MembersAreNamespaceScoped(t *testing.T) {
	mk := func(ns, name string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: name,
				Annotations: map[string]string{
					sustainv1alpha1.PolicyAnnotation:    "p",
					sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
				},
			},
		}
	}
	groups := GroupBarePods([]corev1.Pod{mk("airflow", "a"), mk("airflow", "b"), mk("airflow-staging", "c")})
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if m.Namespace != g.Namespace {
				t.Errorf("group %s/%s has member from namespace %s", g.Namespace, g.Name, m.Namespace)
			}
		}
	}
}

func TestGroupBarePods_NoPolicyAnnotation_NotDiscovered(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "unmanaged",
			Annotations: map[string]string{sustainv1alpha1.OwnerNameAnnotation: "etl-daily"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for a pod with no policy annotation, got %d", len(groups))
	}
}

// GroupBarePods takes the group's PolicyName from the first pod encountered but
// used to admit every pod sharing (namespace, owner-name) that carried ANY
// non-empty policy annotation. Two policies claiming one identity is a genuine
// ambiguity — both map to the same wlrcache.Name — so the group stays claimed
// by the first pod's policy and the mismatched pods are excluded from it
// entirely, not silently absorbed as members.
func TestGroupBarePods_ExcludesPodsOfAnotherPolicy(t *testing.T) {
	mk := func(name, policy string, created time.Time) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         "airflow",
				Name:              name,
				CreationTimestamp: metav1.NewTime(created),
				Annotations: map[string]string{
					sustainv1alpha1.PolicyAnnotation:    policy,
					sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: policy}}},
		}
	}
	base := time.Now()
	groups := GroupBarePods([]corev1.Pod{
		mk("run-1", "mine", base),
		// Newer, so without the exclusion it would also become the
		// representative and donate its containers to the group.
		mk("run-2", "other", base.Add(time.Minute)),
	})

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: a mismatched pod must not fork the identity", len(groups))
	}
	g := groups[0]
	if g.PolicyName != "mine" {
		t.Errorf("PolicyName = %q, want mine", g.PolicyName)
	}
	if len(g.Members) != 1 || g.Members[0].Name != "run-1" {
		t.Errorf("Members = %v, want only run-1", g.Members)
	}
	if g.Representative == nil || g.Representative.Name != "run-1" {
		t.Errorf("Representative must come from the group's own policy, got %v", g.Representative)
	}
	if len(g.Containers) != 1 || g.Containers[0].Name != "mine" {
		t.Errorf("Containers = %v, want the matching pod's containers", g.Containers)
	}
	if len(g.PolicyMismatched) != 1 || g.PolicyMismatched[0].Name != "run-2" {
		t.Errorf("PolicyMismatched = %v, want run-2 recorded so the caller can log it", g.PolicyMismatched)
	}
}
