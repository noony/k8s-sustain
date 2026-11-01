package webhook

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/governance"
)

// Under a selector-less Policy the pre-gate always passes, so every pod CREATE
// of a workload opted in above its pod template reads its owner chain and the
// owner's annotations. A rolling restart is N pods behind the SAME ReplicaSet
// and must collapse to one Get of each, not N.
func TestAdmit_WorkloadLevelOptInCachesOwnerReadsAcrossPodsBehindSameOwner(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "team-a",
		Name:        "web",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	wlr := freshWLR("Deployment", "team-a", "web", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})

	var rsGets, deploymentGets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithObjects(basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), d, deploymentReplicaSet("team-a", "web-abc", "web"), wlr).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *appsv1.ReplicaSet:
					rsGets++
				case *appsv1.Deployment:
					deploymentGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Policies: governance.NewPolicyLister(nil), Client: c}

	for i := range 50 {
		pod := podWithRSOwner("team-a", fmt.Sprintf("web-abc-%d", i), "web-abc", "")
		resp := h.admit(context.Background(), admissionRequestFor(t, pod))
		if !strings.Contains(string(resp.Patch), "/resources") {
			t.Fatalf("admission %d: expected an injection for a workload-level opt-in, got %s", i, resp.Patch)
		}
	}
	if rsGets != 1 || deploymentGets != 1 {
		t.Errorf("expected 1 ReplicaSet and 1 Deployment Get for 50 pods behind the same owner, got %d and %d", rsGets, deploymentGets)
	}
}

// A ReplicaSet or Job name reused under a DIFFERENT top-level owner within the
// TTL must not resolve to the stale owner: the ownerRef UID is the only thing
// that changes for a pod behind the replacement, so it has to be in the key.
func TestPodOwner_KeyIncludesUID(t *testing.T) {
	ctrl := true
	rs1 := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Controller: &ctrl,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(rs1).Build()
	r := admissionReader{h: &Handler{Policies: governance.NewPolicyLister(nil), Client: c}}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc-1",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "uid-1", Controller: &ctrl,
			}},
		},
	}
	kind, name, err := r.PodOwner(context.Background(), pod1)
	if err != nil {
		t.Fatalf("PodOwner (pod1): %v", err)
	}
	if kind != "Deployment" || name != "web" {
		t.Fatalf("pod1: got (%q, %q), want (Deployment, web)", kind, name)
	}

	// Simulate the ReplicaSet being replaced under the same name by one owned
	// by a different Deployment — the pod behind it carries a different
	// ownerRef UID even though kind/name/namespace are identical.
	if err := c.Delete(context.Background(), rs1); err != nil {
		t.Fatalf("delete rs1: %v", err)
	}
	rs2 := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web2", Controller: &ctrl,
			}},
		},
	}
	if err := c.Create(context.Background(), rs2); err != nil {
		t.Fatalf("create rs2: %v", err)
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc-2",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "uid-2", Controller: &ctrl,
			}},
		},
	}
	kind, name, err = r.PodOwner(context.Background(), pod2)
	if err != nil {
		t.Fatalf("PodOwner (pod2): %v", err)
	}
	if kind != "Deployment" || name != "web2" {
		t.Fatalf("stale cache hit: got (%q, %q), want (Deployment, web2) — a different ownerRef UID must not "+
			"share pod1's cache entry", kind, name)
	}
}

// Caching the owner's entire annotations map would balloon the footprint far
// past what ownerAnnotationsCacheMaxEntries suggests — a kubectl-apply-managed
// owner carries last-applied-configuration, up to Kubernetes' 256KB ceiling.
// Only PolicyAnnotation and OptOutAnnotation are ever read back out, so those
// are the only keys cacheableOwnerAnnotations may retain. A read returns the
// very value the cache holds.
func TestWorkloadAnnotations_CacheRetainsOnlyPolicyKeys(t *testing.T) {
	large := strings.Repeat("x", 8192)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a",
		Name:      "web",
		Annotations: map[string]string{
			sustainv1alpha1.PolicyAnnotation:                   "p",
			"kubectl.kubernetes.io/last-applied-configuration": large,
			"some.other/unrelated-annotation":                  "unrelated",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).Build()
	r := admissionReader{h: &Handler{Policies: governance.NewPolicyLister(nil), Client: c}}

	cached, err := r.WorkloadAnnotations(context.Background(), "team-a", "Deployment", "web")
	if err != nil {
		t.Fatalf("WorkloadAnnotations: %v", err)
	}
	if cached[sustainv1alpha1.PolicyAnnotation] != "p" {
		t.Fatalf("annotations = %v, want policy=p", cached)
	}
	if len(cached) > 2 {
		t.Fatalf("cached entry retains %d keys (%v), want at most the 2 keys governance ever reads — "+
			"this fails if the whole annotations map is cached", len(cached), cached)
	}
	if _, ok := cached["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Fatalf("cached entry retains the large unrelated last-applied-configuration annotation: keys=%v", mapKeys(cached))
	}
	if _, ok := cached["some.other/unrelated-annotation"]; ok {
		t.Fatalf("cached entry retains an unrelated annotation: keys=%v", mapKeys(cached))
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
