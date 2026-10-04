// Package inventory turns the cluster's workload objects into identities: the
// unit a recommendation is computed, stored, applied and shown for. The
// controller and the dashboard both read identities from here, so membership,
// containers, age, Departed and the governing Policy have one definition.
package inventory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// Options narrows a snapshot. Every member of an identity shares its
// namespace and kind, so a narrowed snapshot still sees whole identities.
type Options struct {
	// Namespaces limits the snapshot to these namespaces; empty means all.
	Namespaces []string
	// Kinds limits it to these workload.SupportedKinds; empty means all.
	Kinds []string
	// ExcludedNamespaces are governed by no Policy, whatever they opt into.
	ExcludedNamespaces []string
}

// Snapshot is the identities in scope when it was taken, sorted by key.
type Snapshot struct {
	Identities []Identity
	index      map[promclient.WorkloadIdentity]int
}

// Lookup returns the identity with key, if the snapshot has it.
func (s *Snapshot) Lookup(key promclient.WorkloadIdentity) (*Identity, bool) {
	i, ok := s.index[key]
	if !ok {
		return nil, false
	}
	return &s.Identities[i], true
}

// GovernedBy returns the identities policy governs, Departed ones included,
// in key order.
func (s *Snapshot) GovernedBy(policy string) []*Identity {
	var out []*Identity
	for i := range s.Identities {
		if s.Identities[i].Policy == policy {
			out = append(out, &s.Identities[i])
		}
	}
	return out
}

// Identity is one namespace, kind and name, with every live object that
// reports into it.
type Identity struct {
	Key promclient.WorkloadIdentity
	// Members are the live objects of the identity, sorted by name.
	Members []Member
	// Containers and InitContainers are the union of the governed members'
	// containers (of every member when none is governed). A container declared
	// by several members is taken from the newest. A Departed identity's come
	// from its stored observed-resources snapshot.
	Containers     []corev1.Container
	InitContainers []corev1.Container
	// Since is when the identity was first seen: its earliest governed
	// member's creation or its WorkloadRecommendation's, whichever is older.
	Since time.Time
	// Policy is the Policy governing the identity, empty when none does. A
	// Departed identity is governed by its WorkloadRecommendation's Policy
	// while that Policy still covers its namespace and kind.
	Policy string
	// Conflicted is set when members are governed by different Policies; no
	// Policy governs the identity then.
	Conflicted bool
	// Recommendation is the identity's stored WorkloadRecommendation, nil
	// when it has none yet.
	Recommendation *sustainv1alpha1.WorkloadRecommendation
}

// Departed reports whether the identity has no live member left.
func (id *Identity) Departed() bool { return len(id.Members) == 0 }

// MemberPolicies returns the distinct Policies governing the identity's
// members, sorted: one when it is governed, several when it is Conflicted.
func (id *Identity) MemberPolicies() []string {
	var out []string
	for _, m := range id.Members {
		if m.Policy != "" && !slices.Contains(out, m.Policy) {
			out = append(out, m.Policy)
		}
	}
	slices.Sort(out)
	return out
}

// Member is one live object of an identity: a workload object, or a Pod for a
// bare-pod identity. A finished standalone Job and a CronJob's Jobs are never
// members.
type Member struct {
	Object client.Object
	// Policy governs this member: the Policy it opts into, when that Policy
	// exists, manages its kind and selects it. Empty otherwise.
	Policy         string
	Containers     []corev1.Container
	InitContainers []corev1.Container
}

// Take reads every identity in scope through r. Reads go through whatever r
// is backed by; the controller passes its informer cache. A kind the cluster
// does not serve (no Argo Rollouts CRD) has no identities rather than failing
// the snapshot.
func Take(ctx context.Context, r client.Reader, opts Options) (*Snapshot, error) {
	b := &builder{
		reader:     r,
		opts:       opts,
		namespaces: dedupe(opts.Namespaces),
		byKey:      map[promclient.WorkloadIdentity]*Identity{},
		nsAnn:      map[string]map[string]string{},
	}
	if err := b.listPolicies(ctx); err != nil {
		return nil, err
	}
	for _, kind := range workload.SupportedKinds {
		if !b.inScope(kind) {
			continue
		}
		var err error
		if kind == "Pod" {
			err = b.addBarePods(ctx)
		} else {
			err = b.addWorkloads(ctx, kind)
		}
		if err != nil {
			return nil, fmt.Errorf("listing %ss: %w", strings.ToLower(kind), err)
		}
	}
	if err := b.addRecommendations(ctx); err != nil {
		return nil, fmt.Errorf("listing WorkloadRecommendations: %w", err)
	}
	return b.snapshot(), nil
}

type builder struct {
	reader     client.Reader
	opts       Options
	namespaces []string
	policies   map[string]*sustainv1alpha1.Policy
	byKey      map[promclient.WorkloadIdentity]*Identity
	// nsAnn memoises Namespace annotations, read only for objects whose own
	// annotations leave the opt-in undecided.
	nsAnn       map[string]map[string]string
	nsAnnListed bool
}

func (b *builder) inScope(kind string) bool {
	return len(b.opts.Kinds) == 0 || slices.Contains(b.opts.Kinds, kind)
}

func (b *builder) listPolicies(ctx context.Context) error {
	var list sustainv1alpha1.PolicyList
	if err := b.reader.List(ctx, &list); err != nil {
		return fmt.Errorf("listing policies: %w", err)
	}
	b.policies = make(map[string]*sustainv1alpha1.Policy, len(list.Items))
	for i := range list.Items {
		b.policies[list.Items[i].Name] = &list.Items[i]
	}
	return nil
}

// list runs one List per namespace in scope, or one cluster-wide.
func (b *builder) list(ctx context.Context, newList func() client.ObjectList, visit func(client.Object) error) error {
	scopes := b.namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	for _, ns := range scopes {
		l := newList()
		var opts []client.ListOption
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := b.reader.List(ctx, l, opts...); err != nil {
			if meta.IsNoMatchError(err) {
				return nil
			}
			return err
		}
		if err := meta.EachListItem(l, func(o runtime.Object) error { return visit(o.(client.Object)) }); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) addWorkloads(ctx context.Context, kind string) error {
	return b.list(ctx, func() client.ObjectList { return workload.ListForKind(kind) }, func(obj client.Object) error {
		if job, ok := obj.(*batchv1.Job); ok &&
			(workload.IsOwnedByKind(job.OwnerReferences, "CronJob") || workload.JobFinished(job)) {
			return nil
		}
		tmpl, _, ok := workload.PodTemplateOf(obj)
		if !ok {
			return nil
		}
		_, name := workload.ApplyOwnerNameOverride(kind, obj.GetName(), tmpl.Annotations)
		policy, err := b.resolve(ctx, obj.GetNamespace(), tmpl.Annotations, obj.GetAnnotations())
		if err != nil {
			return err
		}
		b.addMember(promclient.WorkloadIdentity{Namespace: obj.GetNamespace(), OwnerKind: kind, OwnerName: name}, Member{
			Object:         obj,
			Policy:         b.govern(policy, kind, obj.GetNamespace(), obj.GetLabels()),
			Containers:     tmpl.Spec.Containers,
			InitContainers: tmpl.Spec.InitContainers,
		})
		return nil
	})
}

// addBarePods adds every pod without a controller owner that names an
// identity through a valid owner-name annotation. A bare pod has no
// template: its own annotations are the most specific opt-in level.
func (b *builder) addBarePods(ctx context.Context) error {
	return b.list(ctx, func() client.ObjectList { return &corev1.PodList{} }, func(obj client.Object) error {
		pod := obj.(*corev1.Pod)
		if metav1.GetControllerOf(pod) != nil {
			return nil
		}
		kind, name := workload.ApplyOwnerNameOverride("", "", pod.Annotations)
		if kind == "" {
			return nil
		}
		policy, err := b.resolve(ctx, pod.Namespace, pod.Annotations, nil)
		if err != nil {
			return err
		}
		b.addMember(promclient.WorkloadIdentity{Namespace: pod.Namespace, OwnerKind: kind, OwnerName: name}, Member{
			Object:         pod,
			Policy:         b.govern(policy, kind, pod.Namespace, pod.Labels),
			Containers:     pod.Spec.Containers,
			InitContainers: pod.Spec.InitContainers,
		})
		return nil
	})
}

func (b *builder) addRecommendations(ctx context.Context) error {
	return b.list(ctx, func() client.ObjectList { return &sustainv1alpha1.WorkloadRecommendationList{} }, func(obj client.Object) error {
		wlr := obj.(*sustainv1alpha1.WorkloadRecommendation)
		ref := wlr.Spec.WorkloadRef
		if !b.inScope(ref.Kind) {
			return nil
		}
		b.identity(promclient.WorkloadIdentity{Namespace: ref.Namespace, OwnerKind: ref.Kind, OwnerName: ref.Name}).Recommendation = wlr
		return nil
	})
}

func (b *builder) identity(key promclient.WorkloadIdentity) *Identity {
	id, ok := b.byKey[key]
	if !ok {
		id = &Identity{Key: key}
		b.byKey[key] = id
	}
	return id
}

func (b *builder) addMember(key promclient.WorkloadIdentity, m Member) {
	id := b.identity(key)
	id.Members = append(id.Members, m)
}

// resolve returns the Policy an object opts into across the three annotation
// levels, reading the Namespace only when the object's own levels leave it
// undecided.
func (b *builder) resolve(ctx context.Context, namespace string, template, object map[string]string) (string, error) {
	if policymatch.DecidesAt(template) || policymatch.DecidesAt(object) {
		name, _ := policymatch.ResolvePolicy(template, object, nil)
		return name, nil
	}
	ns, err := b.namespaceAnnotations(ctx, namespace)
	if err != nil {
		return "", err
	}
	name, _ := policymatch.ResolvePolicy(template, object, ns)
	return name, nil
}

// namespaceAnnotations reads one Namespace per name when the snapshot is
// narrowed to namespaces, and lists them all once otherwise. A missing
// Namespace has no annotations: namespaces are deleted while snapshots are
// taken.
func (b *builder) namespaceAnnotations(ctx context.Context, name string) (map[string]string, error) {
	if a, ok := b.nsAnn[name]; ok || b.nsAnnListed {
		return a, nil
	}
	if len(b.namespaces) == 0 {
		var list corev1.NamespaceList
		if err := b.reader.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("listing namespaces: %w", err)
		}
		for i := range list.Items {
			b.nsAnn[list.Items[i].Name] = list.Items[i].Annotations
		}
		b.nsAnnListed = true
		return b.nsAnn[name], nil
	}
	var ns corev1.Namespace
	if err := b.reader.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	b.nsAnn[name] = ns.Annotations
	return ns.Annotations, nil
}

// govern returns policy when it accepts an object of kind in namespace with
// labels: it exists, manages the kind, and its selector matches. Opting in is
// necessary but not sufficient.
func (b *builder) govern(policy, kind, namespace string, labels map[string]string) string {
	p, ok := b.policies[policy]
	if !ok || p.Spec.RightSizing.Update.Types.ModeForKind(kind) == nil {
		return ""
	}
	if !policymatch.Matches(p, namespace, labels, b.opts.ExcludedNamespaces) {
		return ""
	}
	return policy
}

// governsDeparted is govern for an identity with no member left to carry
// labels: only the namespace half of the selector can be checked.
func (b *builder) governsDeparted(id *Identity) string {
	if id.Recommendation == nil {
		return ""
	}
	p, ok := b.policies[id.Recommendation.Spec.Policy]
	if !ok || p.Spec.RightSizing.Update.Types.ModeForKind(id.Key.OwnerKind) == nil {
		return ""
	}
	if !policymatch.MatchesSelector(p, id.Key.Namespace, nil, b.opts.ExcludedNamespaces, nil) {
		return ""
	}
	return p.Name
}

func (b *builder) snapshot() *Snapshot {
	s := &Snapshot{
		Identities: make([]Identity, 0, len(b.byKey)),
		index:      make(map[promclient.WorkloadIdentity]int, len(b.byKey)),
	}
	for _, id := range b.byKey {
		b.settle(id)
		s.Identities = append(s.Identities, *id)
	}
	slices.SortFunc(s.Identities, func(a, b Identity) int { return promclient.CompareIdentity(a.Key, b.Key) })
	for i := range s.Identities {
		s.index[s.Identities[i].Key] = i
	}
	return s
}

// settle derives everything an identity's members decide together.
func (b *builder) settle(id *Identity) {
	slices.SortFunc(id.Members, func(a, b Member) int {
		return strings.Compare(a.Object.GetName(), b.Object.GetName())
	})
	if id.Departed() {
		id.Policy = b.governsDeparted(id)
		id.Containers, id.InitContainers = wlrcache.ContainersFromObserved(id.Recommendation.Status.ObservedResources)
		id.Since = id.Recommendation.CreationTimestamp.Time
		return
	}

	policies := id.MemberPolicies()
	contributors := id.Members
	switch len(policies) {
	case 0:
	case 1:
		id.Policy = policies[0]
		contributors = membersWhere(id.Members, func(m Member) bool { return m.Policy == id.Policy })
	default:
		id.Conflicted = true
		contributors = membersWhere(id.Members, func(m Member) bool { return m.Policy != "" })
	}
	id.Containers, id.InitContainers = unionContainers(contributors)
	if id.Recommendation != nil {
		id.Since = id.Recommendation.CreationTimestamp.Time
	}
	for _, m := range contributors {
		id.Since = earlier(id.Since, m.Object.GetCreationTimestamp().Time)
	}
}

func membersWhere(members []Member, keep func(Member) bool) []Member {
	var out []Member
	for _, m := range members {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

// unionContainers merges members' containers newest member first, so a
// container several members declare keeps the newest declaration: an older
// member's spec may predate a change the newer ones already run.
func unionContainers(members []Member) (containers, initContainers []corev1.Container) {
	newestFirst := slices.Clone(members)
	slices.SortStableFunc(newestFirst, func(a, b Member) int {
		return cmp.Compare(b.Object.GetCreationTimestamp().UnixNano(), a.Object.GetCreationTimestamp().UnixNano())
	})
	seen := map[string]bool{}
	seenInit := map[string]bool{}
	for _, m := range newestFirst {
		containers = appendUnseen(containers, m.Containers, seen)
		initContainers = appendUnseen(initContainers, m.InitContainers, seenInit)
	}
	return containers, initContainers
}

func appendUnseen(dst, src []corev1.Container, seen map[string]bool) []corev1.Container {
	for _, c := range src {
		if !seen[c.Name] {
			seen[c.Name] = true
			dst = append(dst, c)
		}
	}
	return dst
}

func earlier(a, b time.Time) time.Time {
	if b.IsZero() {
		return a
	}
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// dedupe drops repeated namespaces: a Policy's selector accepts
// [prod, prod], and listing it verbatim would add every object twice.
func dedupe(namespaces []string) []string {
	var out []string
	for _, ns := range namespaces {
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}
