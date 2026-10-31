package governance_test

import (
	"context"
	"errors"
	"sync"
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

// listingReader decides with the Policies a lister reads from a client, the
// way the webhook and the OOM watcher do.
type listingReader struct {
	lister *governance.PolicyLister
	client client.Reader
}

func (r listingReader) Policies(ctx context.Context) (*governance.Policies, error) {
	return r.lister.List(ctx, r.client)
}

func (listingReader) PodOwner(context.Context, *corev1.Pod) (string, string, error) {
	return "", "", errors.New("no owner reads")
}

func (listingReader) WorkloadAnnotations(context.Context, string, string, string) (map[string]string, error) {
	return nil, nil
}

func (listingReader) NamespaceAnnotations(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func webOptingInto(namespace, policy string, labels map[string]string) governance.Subject {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "web"}}
	d.Spec.Template.Labels = labels
	d.Spec.Template.Annotations = map[string]string{sustainv1alpha1.PolicyAnnotation: policy}
	return governance.PodTemplate(d, "Deployment", &d.Spec.Template)
}

func selecting(tier string) *sustainv1alpha1.Policy {
	mode := sustainv1alpha1.UpdateModeOngoing
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": tier}},
			},
			RightSizing: sustainv1alpha1.RightSizingSpec{Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &mode}}},
		},
	}
}

// A lister hands out the Policy it compiled for as long as the stored Policy
// is unchanged, and the new version as soon as it changes or goes.
func TestPolicyListerFollowsEveryChangeToAPolicy(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(selecting("web")).Build()
	r := listingReader{lister: governance.NewPolicyLister([]string{"kube-system"}), client: c}
	web := webOptingInto("team-a", "p", map[string]string{"tier": "web"})

	first, err := governance.Decide(ctx, web, r)
	if err != nil || !first.Governed() {
		t.Fatalf("first decision = %+v, %v; want p governing", first, err)
	}
	again, _ := governance.Decide(ctx, web, r)
	if again.Policy != first.Policy {
		t.Error("an unchanged Policy was compiled again")
	}

	var p sustainv1alpha1.Policy
	if err := c.Get(ctx, client.ObjectKey{Name: "p"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Selector.LabelSelector.MatchLabels["tier"] = "api"
	if err := c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if d, _ := governance.Decide(ctx, web, r); d.Refusal != governance.LabelsNotSelected {
		t.Errorf("after the selector changed: refusal = %q, want %q", d.Refusal, governance.LabelsNotSelected)
	}
	if d, _ := governance.Decide(ctx, webOptingInto("team-a", "p", map[string]string{"tier": "api"}), r); !d.Governed() {
		t.Errorf("after the selector changed: an api pod template is refused %q", d.Refusal)
	}

	if err := c.Delete(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if d, _ := governance.Decide(ctx, web, r); d.Refusal != governance.PolicyMissing {
		t.Errorf("after the Policy went: refusal = %q, want %q", d.Refusal, governance.PolicyMissing)
	}
}

// storedPolicies lists the Policies it holds with the UID and resourceVersion
// they carry, which a fake client would assign itself.
type storedPolicies struct {
	client.Reader
	items  []sustainv1alpha1.Policy
	listed client.ListOptions
}

func (s *storedPolicies) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	s.listed = client.ListOptions{}
	s.listed.ApplyOptions(opts)
	l := list.(*sustainv1alpha1.PolicyList)
	for i := range s.items {
		l.Items = append(l.Items, *s.items[i].DeepCopy())
	}
	return nil
}

// A Policy deleted and recreated under its name is another Policy: it is
// compiled again even at the resourceVersion the deleted one had, where only
// its UID tells them apart.
func TestPolicyListerRecompilesARecreatedPolicy(t *testing.T) {
	ctx := context.Background()
	deleted, recreated := selecting("web"), selecting("api")
	deleted.UID, deleted.ResourceVersion = "deleted", "7"
	recreated.UID, recreated.ResourceVersion = "recreated", "7"
	src := &storedPolicies{items: []sustainv1alpha1.Policy{*deleted}}
	r := listingReader{lister: governance.NewPolicyLister(nil), client: src}
	web := webOptingInto("team-a", "p", map[string]string{"tier": "web"})

	if d, err := governance.Decide(ctx, web, r); err != nil || !d.Governed() {
		t.Fatalf("before the recreate: decision = %+v, %v; want p governing", d, err)
	}
	src.items = []sustainv1alpha1.Policy{*recreated}
	if d, _ := governance.Decide(ctx, web, r); d.Refusal != governance.LabelsNotSelected {
		t.Errorf("after the recreate: web refusal = %q, want %q", d.Refusal, governance.LabelsNotSelected)
	}
	d, _ := governance.Decide(ctx, webOptingInto("team-a", "p", map[string]string{"tier": "api"}), r)
	if !d.Governed() || d.Policy.UID != recreated.UID {
		t.Errorf("after the recreate: api decision = %+v, want the recreated p governing", d)
	}
}

// The webhook lists every Policy on every admission, through an informer
// cache that would otherwise deep-copy each one.
func TestPolicyListerListsWithoutDeepCopies(t *testing.T) {
	src := &storedPolicies{items: []sustainv1alpha1.Policy{*selecting("web")}}
	if _, err := new(governance.PolicyLister).List(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if noCopy := src.listed.UnsafeDisableDeepCopy; noCopy == nil || !*noCopy {
		t.Error("the List asks the cache for deep copies")
	}
}

func TestPolicyListerExcludesItsNamespaces(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(selecting("web")).Build()
	r := listingReader{lister: governance.NewPolicyLister([]string{"kube-system"}), client: c}
	d, err := governance.Decide(context.Background(), webOptingInto("kube-system", "p", map[string]string{"tier": "web"}), r)
	if err != nil || d.Refusal != governance.NamespaceExcluded {
		t.Errorf("decision = %q, %v; want %q", d.Refusal, err, governance.NamespaceExcluded)
	}
}

func TestANilPolicyListerExcludesNoNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(selecting("web")).Build()
	r := listingReader{client: c}
	d, err := governance.Decide(context.Background(), webOptingInto("kube-system", "p", map[string]string{"tier": "web"}), r)
	if err != nil || !d.Governed() {
		t.Errorf("decision = %q, %v; want p governing", d.Refusal, err)
	}
}

func TestPolicyListerReturnsTheListError(t *testing.T) {
	errList := errors.New("boom")
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return errList },
	}).Build()
	if _, err := new(governance.PolicyLister).List(context.Background(), c); !errors.Is(err, errList) {
		t.Errorf("err = %v, want the List error", err)
	}
}

// Admissions list concurrently while Policies change.
func TestPolicyListerIsSafeForConcurrentUse(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(selecting("web")).Build()
	r := listingReader{lister: governance.NewPolicyLister([]string{"kube-system"}), client: c}
	web := webOptingInto("team-a", "p", map[string]string{"tier": "web"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := governance.Decide(ctx, web, r); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 20 {
			var p sustainv1alpha1.Policy
			if err := c.Get(ctx, client.ObjectKey{Name: "p"}, &p); err != nil {
				t.Error(err)
				return
			}
			p.Spec.Selector.LabelSelector.MatchLabels["tier"] = []string{"web", "api"}[i%2]
			if err := c.Update(ctx, &p); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
}
