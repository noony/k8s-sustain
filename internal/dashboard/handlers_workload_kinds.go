package dashboard

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

var supportedWorkloadKinds = workload.SupportedKinds

type containerStatus struct {
	Name          string `json:"name"`
	Init          bool   `json:"init,omitempty"`
	CPURequest    string `json:"cpuRequest"`
	CPULimit      string `json:"cpuLimit"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit"`
}

type coordinationFactors struct {
	Enabled        bool    `json:"enabled"`
	CPUOverhead    float64 `json:"cpuOverhead,omitempty"`
	MemoryOverhead float64 `json:"memoryOverhead,omitempty"`
	CPUReplica     float64 `json:"cpuReplica,omitempty"`
}

// workloadEntry is the kind-agnostic view of a workload: identity plus pod
// template. Name is the resolved identity (owner-name override when present),
// which is also what Prometheus and WorkloadRecommendation are keyed by.
type workloadEntry struct {
	Namespace string
	Name      string
	Template  *corev1.PodTemplateSpec
	// ObjectAnnotations and NamespaceAnnotations are the opt-in levels that
	// are not on the pod template.
	ObjectAnnotations    map[string]string
	NamespaceAnnotations map[string]string
	// ObjectLabels is what the Policy's LabelSelector is matched against.
	ObjectLabels map[string]string
	// Members holds the opt-in and label data of every real object folded
	// into this identity, representative first; nil when a single object backs
	// it. entryMatchesPolicy evaluates opt-in and selector on the same member.
	Members []identityMember
	// CreationTimestamp picks the representative in groupEntriesByIdentity.
	CreationTimestamp time.Time
	// FromRetainedWLR marks an entry synthesized from a retained
	// WorkloadRecommendation. It carries no labels, so only the namespace half
	// of the selector is checked for it.
	FromRetainedWLR bool
}

// identityMember is one real object folded into a workloadEntry identity.
type identityMember struct {
	TemplateAnnotations map[string]string
	ObjectAnnotations   map[string]string
	Labels              map[string]string
}

// entryMatchesPolicy reports whether policy manages entry: some real object
// behind the identity both opts into policy and satisfies its selector, with
// both halves evaluated on the same member. A retained-WLR entry has no
// labels, so only the namespace checks run for it.
func entryMatchesPolicy(policy *sustainv1alpha1.Policy, entry workloadEntry, excludedNamespaces []string) bool {
	if entry.FromRetainedWLR {
		return entry.ResolvedPolicy() == policy.Name &&
			policymatch.MatchesSelector(policy, entry.Namespace, nil, excludedNamespaces, nil)
	}
	if len(entry.Members) == 0 {
		return entry.ResolvedPolicy() == policy.Name &&
			policymatch.Matches(policy, entry.Namespace, entry.ObjectLabels, excludedNamespaces)
	}
	for _, m := range entry.Members {
		name, _ := policymatch.ResolvePolicy(m.TemplateAnnotations, m.ObjectAnnotations, entry.NamespaceAnnotations)
		if name != policy.Name {
			continue
		}
		if policymatch.Matches(policy, entry.Namespace, m.Labels, excludedNamespaces) {
			return true
		}
	}
	return false
}

// ResolvedPolicy returns the Policy this workload opts into, across all three
// annotation levels.
func (e workloadEntry) ResolvedPolicy() string {
	var template map[string]string
	if e.Template != nil {
		template = e.Template.Annotations
	}
	name, _ := policymatch.ResolvePolicy(template, e.ObjectAnnotations, e.NamespaceAnnotations)
	return name
}

func (e workloadEntry) Containers() []corev1.Container {
	if e.Template == nil {
		return nil
	}
	return e.Template.Spec.Containers
}

func (e workloadEntry) InitContainers() []corev1.Container {
	if e.Template == nil {
		return nil
	}
	return e.Template.Spec.InitContainers
}

// listWorkloadsOfKind lists every workload of kind as kind-agnostic entries.
// CronJob-owned Jobs are skipped; they appear under their CronJob. Callers
// looping over kinds must fetch nsAnnotations once and pass the same map in,
// or the cluster-wide Namespace List runs once per kind.
func (s *Server) listWorkloadsOfKind(ctx context.Context, kind string, nsAnnotations map[string]map[string]string, opts ...client.ListOption) ([]workloadEntry, error) {
	if !slices.Contains(supportedWorkloadKinds, kind) {
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
	if kind == "Pod" {
		var list corev1.PodList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		groups := workload.GroupBarePods(list.Items, nsAnnotations)
		out := make([]workloadEntry, len(groups))
		for i, g := range groups {
			out[i] = workloadEntry{
				Namespace:            g.Namespace,
				Name:                 g.Name,
				Template:             barePodGroupTemplate(g),
				ObjectLabels:         g.Labels,
				NamespaceAnnotations: nsAnnotations[g.Namespace],
			}
		}
		return out, nil
	}

	list := workload.ListForKind(kind)
	if err := s.K8sClient.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	out := []workloadEntry{}
	err := meta.EachListItem(list, func(o runtime.Object) error {
		obj := o.(client.Object)
		if job, ok := obj.(*batchv1.Job); ok && workload.IsOwnedByKind(job.OwnerReferences, "CronJob") {
			return nil
		}
		tmpl, _, _ := workload.PodTemplateOf(obj)
		out = append(out, workloadEntry{
			Namespace:            obj.GetNamespace(),
			Name:                 obj.GetName(),
			Template:             tmpl,
			ObjectAnnotations:    obj.GetAnnotations(),
			ObjectLabels:         obj.GetLabels(),
			NamespaceAnnotations: nsAnnotations[obj.GetNamespace()],
			CreationTimestamp:    obj.GetCreationTimestamp().Time,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return groupEntriesByIdentity(out, kind), nil
}

// groupEntriesByIdentity collapses entries sharing an owner-name override onto
// one entry per namespace, keeping the most recently created object as the
// representative. Members is built in fold order, representative first;
// resolveManagingPolicy takes the first matching member, so that order
// decides the displayed policy.
func groupEntriesByIdentity(entries []workloadEntry, kind string) []workloadEntry {
	type accum struct {
		rep   workloadEntry
		extra []identityMember
	}
	best := make(map[string]accum, len(entries))
	var order []string
	for _, e := range entries {
		var annotations map[string]string
		if e.Template != nil {
			annotations = e.Template.Annotations
		}
		_, identity := workload.ApplyOwnerNameOverride(kind, e.Name, annotations)
		key := workloadKey(e.Namespace, kind, identity)
		a, seen := best[key]
		if !seen {
			e.Name = identity
			best[key] = accum{rep: e}
			order = append(order, key)
			continue
		}
		if e.CreationTimestamp.After(a.rep.CreationTimestamp) {
			old := a.rep
			e.Name = identity
			a.extra = append(a.extra, memberOf(old))
			a.rep = e
		} else {
			a.extra = append(a.extra, memberOf(e))
		}
		// accum is stored by value; write the mutated copy back.
		best[key] = a
	}
	out := make([]workloadEntry, 0, len(order))
	for _, key := range order {
		a := best[key]
		rep := a.rep
		if len(a.extra) > 0 {
			rep.Members = append([]identityMember{memberOf(rep)}, a.extra...)
		}
		out = append(out, rep)
	}
	return out
}

// memberOf snapshots e's opt-in and label data as an identityMember.
func memberOf(e workloadEntry) identityMember {
	var template map[string]string
	if e.Template != nil {
		template = e.Template.Annotations
	}
	return identityMember{
		TemplateAnnotations: template,
		ObjectAnnotations:   e.ObjectAnnotations,
		Labels:              e.ObjectLabels,
	}
}

// barePodGroupTemplate synthesizes a PodTemplateSpec for a bare-pod group so
// "Pod" can reuse workloadEntry's accessors.
func barePodGroupTemplate(g workload.BarePodGroup) *corev1.PodTemplateSpec {
	tmpl := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: g.Containers, InitContainers: g.InitContainers},
	}
	if g.Representative != nil {
		tmpl.Annotations = g.Representative.Annotations
		tmpl.Labels = g.Representative.Labels
	}
	return tmpl
}

// getWorkloadEntry fetches one workload by resolved identity. It cannot be a
// client.Get: with an owner-name override, name may not be any real object's
// name, so it lists the kind in the namespace and matches on resolved Name.
func (s *Server) getWorkloadEntry(ctx context.Context, namespace, kind, name string) (workloadEntry, error) {
	// Reject unsupported kinds before paying for the Namespace List below.
	if !slices.Contains(supportedWorkloadKinds, kind) {
		return workloadEntry{}, apierrors.NewNotFound(workload.GroupResourceForKind(kind), name)
	}
	nsAnnotations, err := s.namespaceAnnotations(ctx)
	if err != nil {
		// A namespace read failure must not break detail pages.
		s.Logger.Error(err, "failed to list namespaces; namespace-level policy opt-in will not be resolved")
		nsAnnotations = nil
	}
	entries, err := s.listWorkloadsOfKind(ctx, kind, nsAnnotations, client.InNamespace(namespace))
	if err != nil {
		return workloadEntry{}, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
	}
	if e, ok := s.inactiveWorkloadEntry(ctx, namespace, kind, name); ok {
		return e, nil
	}
	return workloadEntry{}, apierrors.NewNotFound(workload.GroupResourceForKind(kind), name)
}

// inactiveWorkloadEntry reconstructs a workloadEntry from a retained
// WorkloadRecommendation so detail endpoints work for inactive workloads.
func (s *Server) inactiveWorkloadEntry(ctx context.Context, namespace, kind, name string) (workloadEntry, bool) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := s.K8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		s.Logger.Error(err, "failed to list WorkloadRecommendations for inactive fallback", "namespace", namespace)
		return workloadEntry{}, false
	}
	for i := range list.Items {
		wlr := &list.Items[i]
		ref := wlr.Spec.WorkloadRef
		if ref.Kind != kind || ref.Name != name || wlr.Spec.Policy == "" {
			continue
		}
		containers, initContainers := wlrcache.ContainersFromObserved(wlr.Status.ObservedResources)
		return workloadEntry{
			Namespace: namespace,
			Name:      name,
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: wlr.Spec.Policy},
				},
				Spec: corev1.PodSpec{Containers: containers, InitContainers: initContainers},
			},
			FromRetainedWLR: true,
		}, true
	}
	return workloadEntry{}, false
}

// workloadKey builds the "namespace|kind|name" key rows and Prometheus
// label maps are matched on.
func workloadKey(namespace, kind, name string) string {
	return namespace + "|" + kind + "|" + name
}

// paginateRange clamps page/pageSize into a valid [start, end) slice index
// for a list of `total` items. start == end means the page is empty.
func paginateRange(total, page, pageSize int) (start, end int) {
	start = min((page-1)*pageSize, total)
	end = min(start+pageSize, total)
	return
}

// resourceStrings returns the four request/limit strings for a container,
// returning "" for missing or zero values.
func resourceStrings(c corev1.Container) (cpuReq, cpuLim, memReq, memLim string) {
	if req := c.Resources.Requests; req != nil {
		if cpu := req.Cpu(); cpu != nil && !cpu.IsZero() {
			cpuReq = cpu.String()
		}
		if mem := req.Memory(); mem != nil && !mem.IsZero() {
			memReq = mem.String()
		}
	}
	if lim := c.Resources.Limits; lim != nil {
		if cpu := lim.Cpu(); cpu != nil && !cpu.IsZero() {
			cpuLim = cpu.String()
		}
		if mem := lim.Memory(); mem != nil && !mem.IsZero() {
			memLim = mem.String()
		}
	}
	return
}

// containerStatuses concatenates regular and init container statuses; names
// are unique across both lists.
func containerStatuses(containers, initContainers []corev1.Container) []containerStatus {
	out := make([]containerStatus, 0, len(containers)+len(initContainers))
	for _, c := range containers {
		out = append(out, containerStatusFor(c, false))
	}
	for _, c := range initContainers {
		out = append(out, containerStatusFor(c, true))
	}
	return out
}

func containerStatusFor(c corev1.Container, isInit bool) containerStatus {
	cpuReq, cpuLim, memReq, memLim := resourceStrings(c)
	return containerStatus{
		Name:          c.Name,
		Init:          isInit,
		CPURequest:    cpuReq,
		CPULimit:      cpuLim,
		MemoryRequest: memReq,
		MemoryLimit:   memLim,
	}
}

// assembleCoordinationFactors maps {resource|kind: value} series onto a
// coordinationFactors payload.
func assembleCoordinationFactors(byLabels map[string]float64) *coordinationFactors {
	out := &coordinationFactors{Enabled: true}
	for k, v := range byLabels {
		switch k {
		case "cpu|overhead":
			out.CPUOverhead = v
		case "memory|overhead":
			out.MemoryOverhead = v
		case "cpu|replica":
			out.CPUReplica = v
		}
	}
	return out
}

func kindEnabledInPolicy(p *sustainv1alpha1.Policy, kind string) bool {
	return p.Spec.RightSizing.Update.Types.ModeForKind(kind) != nil
}

// updateModeForKind returns the policy's per-kind update mode, or nil.
func updateModeForKind(p *sustainv1alpha1.Policy, kind string) *sustainv1alpha1.UpdateMode {
	return p.Spec.RightSizing.Update.Types.ModeForKind(kind)
}
