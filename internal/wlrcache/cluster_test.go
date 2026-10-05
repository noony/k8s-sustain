package wlrcache_test

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// cluster is an API server behind a client that behaves like the
// informer-backed one every writer runs against in the one way
// fake.NewClientBuilder cannot express: it is not read-your-writes, so a Get
// straight after a Create races the watch event and returns NotFound. It fails
// every Get of a created-but-unwarmed key and serves every other read. Without
// it the bug class is invisible: code that re-read after Create shipped,
// because every test it had used a read-your-writes fake.
//
// It also counts creates and status writes, the units the lifecycle is
// budgeted in.
type cluster struct {
	client.Client
	store client.Client

	mu           sync.Mutex
	cold         map[client.ObjectKey]struct{}
	creates      int
	statusWrites int
}

func newCluster(t *testing.T, objs ...client.Object) *cluster {
	t.Helper()
	s := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := &cluster{
		store: fake.NewClientBuilder().WithScheme(s).
			WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
			WithObjects(objs...).Build(),
		cold: map[client.ObjectKey]struct{}{},
	}
	c.Client = interceptor.NewClient(c.store.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.creates++
			c.cold[client.ObjectKeyFromObject(obj)] = struct{}{}
			return nil
		},
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			c.mu.Lock()
			_, cold := c.cold[key]
			c.mu.Unlock()
			if cold {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: sustainv1alpha1.GroupVersion.Group, Resource: "workloadrecommendations"},
					key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			c.mu.Lock()
			c.statusWrites++
			c.mu.Unlock()
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	return c
}

// warm models the watch events landing.
func (c *cluster) warm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cold = map[client.ObjectKey]struct{}{}
}

// writes returns the creates and status writes issued so far.
func (c *cluster) writes() (creates, statusWrites int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates, c.statusWrites
}

// stored reads the persisted object for ref, past any lag; nil when absent.
func (c *cluster) stored(t *testing.T, ref sustainv1alpha1.WorkloadReference) *sustainv1alpha1.WorkloadRecommendation {
	t.Helper()
	var wlr sustainv1alpha1.WorkloadRecommendation
	key := client.ObjectKey{Namespace: ref.Namespace, Name: wlrcache.Name(ref.Kind, ref.Name)}
	if err := c.store.Get(context.Background(), key, &wlr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("get %s: %v", key, err)
	}
	return &wlr
}

// mustStored is stored for an object the test expects to exist.
func (c *cluster) mustStored(t *testing.T, ref sustainv1alpha1.WorkloadReference) *sustainv1alpha1.WorkloadRecommendation {
	t.Helper()
	wlr := c.stored(t, ref)
	if wlr == nil {
		t.Fatalf("no WorkloadRecommendation for %+v", ref)
	}
	return wlr
}
