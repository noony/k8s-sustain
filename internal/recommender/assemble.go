package recommender

import (
	"context"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
)

// Autoscalers reads which autoscaler targets each member of an identity.
// autoscaler.NamespacedSnapshot reads them from the cluster.
type Autoscalers interface {
	ForIdentity(ctx context.Context, namespace, kind string, members []string) (autoscaler.Group, error)
}

// Assembled is one identity's Request, or why its inputs could not be read.
type Assembled struct {
	// Request is what Compute takes. Unset when Err is.
	Request Request
	// Autoscaler is what targets each of the identity's members. The Request
	// is shaped against its Info.
	Autoscaler autoscaler.Group
	Err        error
}

// autoscalerReadConcurrency bounds how many namespaces Assemble reads the
// autoscalers of at once. It is a goroutine bound only: a namespace's first
// read Lists its HPAs and ScaledObjects, later ones read that snapshot.
const autoscalerReadConcurrency = 4

// Assemble turns each identity into the Request spec makes of it, in order:
// the controller and the dashboard's Simulations both go through here, so they
// hand Compute the same Request. It fetches every identity's inputs in one
// batch under spec's resources, defaults made explicit (WithDefaults), while
// it reads the autoscaler of every member whose containers make the identity's
// (a departed identity has none), and leaves init containers out when spec
// excludes them. An identity with no containers at all, one the inventory has
// not seen, is recommended for every container its inputs report. Every
// Request computes as of one instant read from now once the fetch returned;
// a caller whose inputs are decorated with WithLiveOOMs passes the decorator's
// clock.
func Assemble(ctx context.Context, inputs InputsFetcher, autoscalers Autoscalers, now func() time.Time, spec sustainv1alpha1.RightSizingSpec, ids []*inventory.Identity) []Assembled {
	resources := WithDefaults(spec.ResourcesConfigs)
	out := make([]Assembled, len(ids))
	reqs := make([]InputsRequest, len(ids))
	for i, id := range ids {
		containers := ContainersOf(spec, id)
		out[i].Request = Request{
			containers:   containers,
			resources:    resources,
			coordination: spec.AutoscalerCoordination,
			since:        id.Since,
		}
		reqs[i] = InputsRequest{Identity: id.Key, Containers: len(containers)}
	}

	read := readAutoscalers(ctx, autoscalers, ids)
	fetched := inputs.FetchInputs(ctx, resources, reqs)
	// Taken after the fetch, which can take minutes: kills it merged in must
	// not postdate the instant they are computed as of.
	at := now()
	groups := read()
	for i, id := range ids {
		group := groups[i]
		f := fetched[id.Key]
		if f.Err != nil {
			out[i] = Assembled{Autoscaler: group, Err: f.Err}
			continue
		}
		out[i].Autoscaler = group
		req := &out[i].Request
		req.autoInfo, req.inputs, req.now = group.Info, f.Inputs, at
		if len(id.Containers) == 0 && len(id.InitContainers) == 0 {
			req.containers = observedContainers(f.Inputs)
		}
	}
	return out
}

// readAutoscalers starts reading the autoscaler of each identity, every
// namespace's in a goroutine of its own, at most autoscalerReadConcurrency at
// once, and returns what waits for them all and answers them in ids' order.
func readAutoscalers(ctx context.Context, autoscalers Autoscalers, ids []*inventory.Identity) (wait func() []autoscaler.Group) {
	groups := make([]autoscaler.Group, len(ids))
	byNamespace := map[string][]int{}
	var namespaces []string
	for i, id := range ids {
		ns := id.Key.Namespace
		if _, ok := byNamespace[ns]; !ok {
			namespaces = append(namespaces, ns)
		}
		byNamespace[ns] = append(byNamespace[ns], i)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var g errgroup.Group
		g.SetLimit(autoscalerReadConcurrency)
		for _, ns := range namespaces {
			g.Go(func() error {
				readNamespace(ctx, autoscalers, ns, ids, byNamespace[ns], groups)
				return nil
			})
		}
		_ = g.Wait()
	}()
	return func() []autoscaler.Group {
		<-done
		return groups
	}
}

// readNamespace reads, in order, the autoscaler of the identities of ids at
// the indexes at, all of namespace ns, into the same indexes of groups. It
// stops reading the namespace once a read failed, answering its other
// identities with that failure: NamespacedSnapshot caches no failure, so
// every identity of an unreadable namespace would List its HPAs and
// ScaledObjects again. The failure is logged once.
func readNamespace(ctx context.Context, autoscalers Autoscalers, ns string, ids []*inventory.Identity, at []int, groups []autoscaler.Group) {
	var failed *autoscaler.Group
	for _, i := range at {
		if failed != nil {
			groups[i] = *failed
			continue
		}
		group, err := autoscalerOf(ctx, autoscalers, ids[i])
		groups[i] = group
		if err != nil {
			failed = &group
			log.FromContext(ctx).Error(err, "reading the namespace's autoscalers failed; its identities are recommended uncoordinated",
				"namespace", ns)
		}
	}
}

// ContainersOf is the containers spec recommends of id: its containers, and
// its init containers unless spec excludes them. Assemble recommends these;
// the controller and the dashboard apply excludeInitContainers here and
// nowhere else.
func ContainersOf(spec sustainv1alpha1.RightSizingSpec, id *inventory.Identity) []corev1.Container {
	if spec.ExcludeInitContainers {
		return id.Containers
	}
	return slices.Concat(id.Containers, id.InitContainers)
}

// autoscalerOf reads the autoscaler of each member the identity's containers
// come from. A departed identity has none, and a failed read leaves the
// identity uncoordinated.
func autoscalerOf(ctx context.Context, autoscalers Autoscalers, id *inventory.Identity) (autoscaler.Group, error) {
	members := id.Contributors()
	if len(members) == 0 {
		return autoscaler.Group{Info: autoscaler.Info{Kind: autoscaler.KindNone}}, nil
	}
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Object.GetName()
	}
	return autoscalers.ForIdentity(ctx, id.Key.Namespace, id.Key.OwnerKind, names)
}

// WithDefaults returns cfg with every setting the recommender defaults made
// explicit: each resource's window, its percentile and headroom unless its
// request is kept, and its limit strategy, keepLimit, when none is set.
func WithDefaults(cfg sustainv1alpha1.ResourcesConfigs) sustainv1alpha1.ResourcesConfigs {
	out := *cfg.DeepCopy()
	for _, rc := range []*sustainv1alpha1.ResourceConfig{&out.CPU, &out.Memory} {
		rc.Window = ResourceWindow(rc.Window)
		if !rc.Requests.KeepRequest {
			if rc.Requests.Percentile == nil {
				rc.Requests.Percentile = ptr.To[int32](defaultPercentile)
			}
			if rc.Requests.Headroom == nil {
				rc.Requests.Headroom = ptr.To[int32](0)
			}
		}
		if strategyOf(rc.Limits).name == stageKeepLimit {
			rc.Limits.KeepLimit = true
		}
	}
	return out
}
