package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// supportedWorkloadKinds is the canonical ordering used by responses that
// iterate over every workload kind the dashboard recognises. "Pod" identifies
// bare-pod identities formed via api/v1alpha1.OwnerNameAnnotation — see
// workload.GroupBarePods.
var supportedWorkloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "Rollout", "CronJob", "Job", "Pod"}

// containerStatus and coordinationFactors are referenced by multiple handler
// files; their definitions are kept here as the workload-shaped shared types.

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

// workloadEntry is the kind-agnostic view of a workload object: just the
// identity and the pod template that owns its resource decisions. Handlers
// consume it instead of branching on the concrete Kubernetes type.
//
// Name is the resolved identity (the k8s.sustain.io/owner-name override
// value when the pod template carries a valid one, otherwise the object's
// real Kubernetes name) — see groupEntriesByIdentity. This is deliberate:
// Prometheus and the WorkloadRecommendation are keyed by the same resolved
// identity (internal/workload.ApplyOwnerNameOverride), so addressing,
// listing, and signal lookups all need to agree on it, not the real object
// name, or recommendations/risk/drift would silently fail to match for any
// overridden workload.
type workloadEntry struct {
	Namespace string
	Name      string
	Template  *corev1.PodTemplateSpec
	OwnerRefs []metav1.OwnerReference
	// CreationTimestamp is read by groupEntriesByIdentity to pick the most
	// recently created object as the representative when multiple real
	// objects share one overridden identity. Unused once grouping is done.
	CreationTimestamp time.Time
}

func (e workloadEntry) PolicyAnnotation() string {
	if e.Template == nil {
		return ""
	}
	return e.Template.Annotations[sustainv1alpha1.PolicyAnnotation]
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

// listWorkloadsOfKind lists every workload of the given kind, returning
// kind-agnostic entries. For "Job" it skips Jobs spawned by a CronJob — those
// appear under their owning CronJob row, and metrics attribute their pods to
// owner_kind=CronJob.
//
// Every kind except "Pod" goes through groupEntriesByIdentity: real objects
// whose pod template carries a k8s.sustain.io/owner-name override collapse
// onto one entry named by the override (see workloadEntry.Name), matching
// what Prometheus/WorkloadRecommendation already key by. "Pod" doesn't need
// the extra pass — workload.GroupBarePods already produces final identities.
func (s *Server) listWorkloadsOfKind(ctx context.Context, kind string, opts ...client.ListOption) ([]workloadEntry, error) {
	switch kind {
	case "Deployment":
		var list appsv1.DeploymentList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			d := &list.Items[i]
			out[i] = workloadEntry{Namespace: d.Namespace, Name: d.Name, Template: &d.Spec.Template, OwnerRefs: d.OwnerReferences, CreationTimestamp: d.CreationTimestamp.Time}
		}
		return groupEntriesByIdentity(out, kind), nil
	case "StatefulSet":
		var list appsv1.StatefulSetList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			st := &list.Items[i]
			out[i] = workloadEntry{Namespace: st.Namespace, Name: st.Name, Template: &st.Spec.Template, OwnerRefs: st.OwnerReferences, CreationTimestamp: st.CreationTimestamp.Time}
		}
		return groupEntriesByIdentity(out, kind), nil
	case "DaemonSet":
		var list appsv1.DaemonSetList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			ds := &list.Items[i]
			out[i] = workloadEntry{Namespace: ds.Namespace, Name: ds.Name, Template: &ds.Spec.Template, OwnerRefs: ds.OwnerReferences, CreationTimestamp: ds.CreationTimestamp.Time}
		}
		return groupEntriesByIdentity(out, kind), nil
	case "Rollout":
		var list rolloutsv1alpha1.RolloutList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			r := &list.Items[i]
			out[i] = workloadEntry{Namespace: r.Namespace, Name: r.Name, Template: &r.Spec.Template, OwnerRefs: r.OwnerReferences, CreationTimestamp: r.CreationTimestamp.Time}
		}
		return groupEntriesByIdentity(out, kind), nil
	case "CronJob":
		var list batchv1.CronJobList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			cj := &list.Items[i]
			out[i] = workloadEntry{Namespace: cj.Namespace, Name: cj.Name, Template: &cj.Spec.JobTemplate.Spec.Template, OwnerRefs: cj.OwnerReferences, CreationTimestamp: cj.CreationTimestamp.Time}
		}
		return groupEntriesByIdentity(out, kind), nil
	case "Job":
		var list batchv1.JobList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, 0, len(list.Items))
		for i := range list.Items {
			j := &list.Items[i]
			if workload.IsOwnedByKind(j.OwnerReferences, "CronJob") {
				continue
			}
			out = append(out, workloadEntry{Namespace: j.Namespace, Name: j.Name, Template: &j.Spec.Template, OwnerRefs: j.OwnerReferences, CreationTimestamp: j.CreationTimestamp.Time})
		}
		return groupEntriesByIdentity(out, kind), nil
	case "Pod":
		var list corev1.PodList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		groups := workload.GroupBarePods(list.Items)
		out := make([]workloadEntry, len(groups))
		for i, g := range groups {
			out[i] = workloadEntry{Namespace: g.Namespace, Name: g.Name, Template: barePodGroupTemplate(g)}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

// groupEntriesByIdentity collapses entries whose pod template carries the
// same k8s.sustain.io/owner-name override onto one entry, keeping the most
// recently created one as the representative (its Template/OwnerRefs are
// what gets displayed) and renaming it to the resolved identity. Entries
// without a valid override pass through unchanged — their own name is
// already their identity. kind is the real declared kind (e.g. "Deployment"),
// passed to workload.ApplyOwnerNameOverride; since it's never empty here, the
// override never changes kind, only name.
//
// Order of the returned slice follows first-seen identity, for stable
// pagination across calls against an unchanged object set.
func groupEntriesByIdentity(entries []workloadEntry, kind string) []workloadEntry {
	best := make(map[string]workloadEntry, len(entries))
	var order []string
	for _, e := range entries {
		var annotations map[string]string
		if e.Template != nil {
			annotations = e.Template.Annotations
		}
		_, identity := workload.ApplyOwnerNameOverride(kind, e.Name, annotations)
		cur, seen := best[identity]
		if !seen || e.CreationTimestamp.After(cur.CreationTimestamp) {
			if !seen {
				order = append(order, identity)
			}
			e.Name = identity
			best[identity] = e
		}
	}
	out := make([]workloadEntry, 0, len(order))
	for _, identity := range order {
		out = append(out, best[identity])
	}
	return out
}

// barePodGroupTemplate synthesizes the PodTemplateSpec workloadEntry expects
// for a bare-pod group: containers/init-containers come from the group's
// representative pod (the most recently created one), and annotations/labels
// (read by workloadEntry.PolicyAnnotation and the policy-selector label match)
// come from the same pod. There is no real pod template for a bare pod — this
// exists purely so "Pod" can reuse workloadEntry's existing accessors instead
// of every caller branching on kind.
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

// getWorkloadEntry fetches a single workload by its resolved identity name
// and returns it as a workloadEntry. Used wherever a handler needs the pod
// template, container list, or policy annotation for one specific entry.
//
// This cannot be a direct client.Get by name: when a pod template carries a
// k8s.sustain.io/owner-name override, `name` is the override value, not any
// real object's Kubernetes name (there may be no object actually named
// that — e.g. two real Deployments grouped under one shared identity). So
// this lists every object of kind in the namespace (via listWorkloadsOfKind,
// which already applies the same override resolution) and finds the entry
// whose resolved Name matches.
func (s *Server) getWorkloadEntry(ctx context.Context, namespace, kind, name string) (workloadEntry, error) {
	entries, err := s.listWorkloadsOfKind(ctx, kind, client.InNamespace(namespace))
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
	return workloadEntry{}, apierrors.NewNotFound(groupResourceForKind(kind), name)
}

// groupResourceForKind maps a dashboard workload kind to the GroupResource
// used to build a NotFound error, since getWorkloadEntry no longer performs
// a typed client.Get that would otherwise supply one automatically.
func groupResourceForKind(kind string) schema.GroupResource {
	switch kind {
	case "Deployment":
		return schema.GroupResource{Group: "apps", Resource: "deployments"}
	case "StatefulSet":
		return schema.GroupResource{Group: "apps", Resource: "statefulsets"}
	case "DaemonSet":
		return schema.GroupResource{Group: "apps", Resource: "daemonsets"}
	case "Rollout":
		return schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"}
	case "CronJob":
		return schema.GroupResource{Group: "batch", Resource: "cronjobs"}
	case "Job":
		return schema.GroupResource{Group: "batch", Resource: "jobs"}
	default:
		return corev1.Resource("pods")
	}
}

// inactiveWorkloadEntry reconstructs a workloadEntry from a retained
// WorkloadRecommendation so detail endpoints keep working for inactive
// workloads the list links to. The synthesized template carries the policy
// annotation and the observed container resources; the Prometheus-driven
// panels (metrics, recommendations) work off identity and history, which
// both outlive the object. ok is false when no matching WLR exists.
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
		var containers, initContainers []corev1.Container
		for cname, res := range wlr.Status.ObservedResources {
			c := corev1.Container{Name: cname, Resources: requirementsFromObserved(res)}
			if res.Init {
				initContainers = append(initContainers, c)
			} else {
				containers = append(containers, c)
			}
		}
		sort.Slice(containers, func(a, b int) bool { return containers[a].Name < containers[b].Name })
		sort.Slice(initContainers, func(a, b int) bool { return initContainers[a].Name < initContainers[b].Name })
		return workloadEntry{
			Namespace: namespace,
			Name:      name,
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: wlr.Spec.Policy},
				},
				Spec: corev1.PodSpec{Containers: containers, InitContainers: initContainers},
			},
		}, true
	}
	return workloadEntry{}, false
}

// requirementsFromObserved rebuilds ResourceRequirements from the snapshot.
func requirementsFromObserved(res sustainv1alpha1.ObservedContainerResources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	set := func(dst *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity) {
		if q == nil {
			return
		}
		if *dst == nil {
			*dst = corev1.ResourceList{}
		}
		(*dst)[name] = *q
	}
	set(&out.Requests, corev1.ResourceCPU, res.CPURequest)
	set(&out.Requests, corev1.ResourceMemory, res.MemoryRequest)
	set(&out.Limits, corev1.ResourceCPU, res.CPULimit)
	set(&out.Limits, corev1.ResourceMemory, res.MemoryLimit)
	return out
}

// workloadKey assembles the "namespace|kind|name" key used to address a
// workload in Prometheus signal maps. Identically-named workloads in different
// namespaces stay distinct.
func workloadKey(namespace, kind, name string) string {
	return namespace + "|" + kind + "|" + name
}

// splitWorkloadKey is the inverse of workloadKey. Missing components come
// back empty; any extra "|" separators stay in the name component.
func splitWorkloadKey(key string) (namespace, kind, name string) {
	parts := strings.SplitN(key, "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
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

// containerStatuses concatenates regular and init container statuses for a
// pod template. Container names are unique across both lists in Kubernetes,
// so callers can safely key the result by name.
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

// workloadSignals holds the Prometheus-derived per-workload state that
// dashboard responses overlay onto their list rows (risk badge, drift,
// autoscaler presence, and coordination factors).
type workloadSignals struct {
	RiskState           string
	DriftPercent        float64
	AutoscalerPresent   bool
	CoordinationFactors *coordinationFactors
}

// fetchWorkloadSignals batches the five signal queries (oom, drift, blocked,
// autoscaler, coordination factors) for every workload at once, then indexes
// the results per row in memory. Returns a map keyed by
// workloadKey(namespace, kind, name) covering every requested key. The
// coordination-factor query is grouped by namespace/owner_kind/owner_name plus
// resource/kind so a single round-trip covers all rows; only rows with an
// autoscaler present get factors, matching the prior per-row condition.
func (s *Server) fetchWorkloadSignals(ctx context.Context, keys []string) map[string]workloadSignals {
	if len(keys) == 0 {
		return nil
	}
	// The OOM rule is per-container; re-aggregate to workload level so one
	// series per workload reaches the keyed map (otherwise a 0-count sibling
	// container could overwrite an OOMed one).
	oom, _ := s.PromClient.QueryByLabels(ctx, fmt.Sprintf("sum by (namespace, owner_kind, owner_name) (%s)", promclient.MetricWorkloadOOM24h), "namespace", "owner_kind", "owner_name")
	drift, _ := s.PromClient.QueryByLabels(ctx, fmt.Sprintf("max by (namespace, owner_kind, owner_name) (abs(1 - %s))", promclient.MetricWorkloadDriftRatio), "namespace", "owner_kind", "owner_name")
	blocked, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricWorkloadRetryState+" == 1", "namespace", "owner_kind", "owner_name")
	autoscaler, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricAutoscalerPresent, "namespace", "owner_kind", "owner_name")
	// Single batched coordination-factor query for every workload. Keyed by
	// namespace|owner_kind|owner_name|resource|kind; the per-workload prefix
	// (first three labels) matches workloadKey, and the resource|kind suffix
	// selects which factor field each series fills.
	coord, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricCoordinationFactor, "namespace", "owner_kind", "owner_name", "resource", "kind")

	out := make(map[string]workloadSignals, len(keys))
	for _, key := range keys {
		sig := workloadSignals{AutoscalerPresent: autoscaler[key] > 0}
		if d, ok := drift[key]; ok {
			sig.DriftPercent = d * 100
		}
		switch {
		case oom[key] > 0:
			sig.RiskState = "at-risk"
		case blocked[key] > 0:
			sig.RiskState = "blocked"
		case sig.DriftPercent > 10:
			sig.RiskState = "drifted"
		default:
			sig.RiskState = "safe"
		}
		if sig.AutoscalerPresent {
			sig.CoordinationFactors = coordinationFactorsFor(coord, key)
		}
		out[key] = sig
	}
	return out
}

// coordinationFactorsFor extracts one workload's coordination factors from the
// batched coordination-factor map (keyed namespace|owner_kind|owner_name|
// resource|kind). prefix is the workloadKey (namespace|kind|name); since the
// metric's owner_kind/owner_name correspond to kind/name, the series keys share
// that three-component prefix followed by |resource|kind. Returns nil when no
// series exist for this workload, matching fetchCoordinationFactors.
func coordinationFactorsFor(coord map[string]float64, prefix string) *coordinationFactors {
	byLabels := map[string]float64{}
	for _, suffix := range []string{"cpu|overhead", "memory|overhead", "cpu|replica"} {
		if v, ok := coord[prefix+"|"+suffix]; ok {
			byLabels[suffix] = v
		}
	}
	if len(byLabels) == 0 {
		return nil
	}
	return assembleCoordinationFactors(byLabels)
}

// assembleCoordinationFactors maps a {resource|kind: value} series map (e.g.
// "cpu|overhead", "memory|overhead", "cpu|replica") onto a coordinationFactors
// payload. Unknown keys are ignored; missing keys leave their field at zero.
// Callers are responsible for the nil-vs-non-nil decision before calling.
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

// fetchCoordinationFactors queries `k8s_sustain_coordination_factor` for one
// workload and assembles a coordinationFactors payload describing the per-
// resource overhead and replica correction factors that the controller and
// webhook applied. Returns nil when no series exist for this workload.
func (s *Server) fetchCoordinationFactors(ctx context.Context, namespace, kind, name string) *coordinationFactors {
	expr := fmt.Sprintf(
		`%s{namespace=%q,owner_kind=%q,owner_name=%q}`,
		promclient.MetricCoordinationFactor, namespace, kind, name,
	)
	byLabels, err := s.PromClient.QueryByLabels(ctx, expr, "resource", "kind")
	if err != nil || len(byLabels) == 0 {
		return nil
	}
	return assembleCoordinationFactors(byLabels)
}

// kindEnabledInPolicy reports whether a policy opts in to the given workload
// kind via its RightSizing.Update.Types map.
func kindEnabledInPolicy(p *sustainv1alpha1.Policy, kind string) bool {
	return p.Spec.RightSizing.Update.Types.ModeForKind(kind) != nil
}

// updateModeForKind returns the policy's per-kind update mode pointer, or nil
// if the policy doesn't opt this kind in.
func updateModeForKind(p *sustainv1alpha1.Policy, kind string) *sustainv1alpha1.UpdateMode {
	return p.Spec.RightSizing.Update.Types.ModeForKind(kind)
}
