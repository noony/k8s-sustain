package workload

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPodOwnedByWorkload covers the ownerRef chains the recycle path relies
// on: direct controller refs (StatefulSet/DaemonSet/Job), the indirect
// pod → ReplicaSet → Deployment chain, orphan pods, and a vanished
// ReplicaSet (treated as unowned rather than an error).
func TestPodOwnedByWorkload(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	rsOwnedByTarget := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-abc",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: "web", UID: "dep-uid", Controller: ptr.To(true),
			}},
		},
	}
	rsOwnedByOther := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "other-abc",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment", Name: "other", UID: "other-uid", Controller: ptr.To(true),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rsOwnedByTarget, rsOwnedByOther).Build()

	podWithRef := func(kind, name string, uid types.UID, controller bool) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "p",
				OwnerReferences: []metav1.OwnerReference{{
					Kind: kind, Name: name, UID: uid, Controller: ptr.To(controller),
				}},
			},
		}
	}

	tests := []struct {
		name string
		pod  *corev1.Pod
		uid  types.UID
		want bool
	}{
		{"orphan pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p"}}, "sts-uid", false},
		{"non-controller ref only", podWithRef("StatefulSet", "web", "sts-uid", false), "sts-uid", false},
		{"direct match", podWithRef("StatefulSet", "web", "sts-uid", true), "sts-uid", true},
		{"direct mismatch", podWithRef("StatefulSet", "web2", "other-sts-uid", true), "sts-uid", false},
		{"replicaset chain match", podWithRef("ReplicaSet", "web-abc", "rs-uid", true), "dep-uid", true},
		{"replicaset chain mismatch", podWithRef("ReplicaSet", "other-abc", "rs-uid", true), "dep-uid", false},
		{"replicaset gone", podWithRef("ReplicaSet", "vanished", "rs-uid", true), "dep-uid", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PodOwnedByWorkload(context.Background(), c, tt.pod, tt.uid, map[string]bool{})
			if err != nil {
				t.Fatalf("PodOwnedByWorkload: %v", err)
			}
			if got != tt.want {
				t.Errorf("PodOwnedByWorkload = %v, want %v", got, tt.want)
			}
		})
	}
}
