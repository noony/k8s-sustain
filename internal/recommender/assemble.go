package recommender

import (
	"context"
	"slices"
	"time"

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
	// Err is why the identity's inputs could not be read.
	Err error
}

// Assemble turns each identity into the Request spec makes of it, in order:
// the controller and the dashboard's Simulations both go through here, so they
// hand Compute the same Request. It reads the autoscaler of every member whose
// containers make the identity's (a departed identity has none), fetches every
// identity's inputs in one batch under spec's resources, defaults made
// explicit (WithDefaults), and leaves init containers out when spec excludes
// them. An identity with no containers at all, one the inventory has not seen,
// is recommended for every container its inputs report.
func Assemble(ctx context.Context, inputs InputsFetcher, autoscalers Autoscalers, spec sustainv1alpha1.RightSizingSpec, ids []*inventory.Identity) []Assembled {
	resources := WithDefaults(spec.ResourcesConfigs)
	out := make([]Assembled, len(ids))
	reqs := make([]InputsRequest, len(ids))
	reads := readsOnce{autoscalers: autoscalers, failed: map[string]failedRead{}}
	for i, id := range ids {
		containers := ContainersOf(spec, id)
		group, _ := autoscalerOf(ctx, reads, id)
		out[i] = Assembled{
			Request: Request{
				containers:   containers,
				resources:    resources,
				coordination: spec.AutoscalerCoordination,
				autoInfo:     group.Info,
				since:        id.Since,
			},
			Autoscaler: group,
		}
		reqs[i] = InputsRequest{Identity: id.Key, Containers: len(containers)}
	}

	fetched := inputs.FetchInputs(ctx, resources, reqs)
	// Taken after the fetch, which can take minutes: kills it merged in must
	// not postdate the instant they are computed as of.
	now := time.Now()
	for i, id := range ids {
		f := fetched[id.Key]
		if f.Err != nil {
			out[i] = Assembled{Autoscaler: out[i].Autoscaler, Err: f.Err}
			continue
		}
		req := &out[i].Request
		req.inputs, req.now = f.Inputs, now
		if len(id.Containers) == 0 && len(id.InitContainers) == 0 {
			req.containers = observedContainers(f.Inputs)
		}
	}
	return out
}

// readsOnce stops reading a namespace's autoscalers once a read of it failed,
// answering its other identities with that failure: NamespacedSnapshot caches
// no failure, so every identity of an unreadable namespace would List its
// HPAs and ScaledObjects again. The failure is logged once.
type readsOnce struct {
	autoscalers Autoscalers
	failed      map[string]failedRead
}

type failedRead struct {
	group autoscaler.Group
	err   error
}

func (r readsOnce) ForIdentity(ctx context.Context, namespace, kind string, members []string) (autoscaler.Group, error) {
	if f, ok := r.failed[namespace]; ok {
		return f.group, f.err
	}
	group, err := r.autoscalers.ForIdentity(ctx, namespace, kind, members)
	if err != nil {
		r.failed[namespace] = failedRead{group: group, err: err}
		log.FromContext(ctx).Error(err, "reading the namespace's autoscalers failed; its identities are recommended uncoordinated",
			"namespace", namespace)
	}
	return group, err
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
		if l := rc.Limits; !l.EqualsToRequest && !l.KeepLimit && !l.KeepLimitRequestRatio && !l.NoLimit && l.RequestsLimitsRatio == nil {
			rc.Limits.KeepLimit = true
		}
	}
	return out
}
