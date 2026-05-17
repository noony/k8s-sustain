package dashboard

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// supportedWorkloadKinds is the canonical ordering used by responses that
// iterate over every workload kind the dashboard recognises.
var supportedWorkloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job"}

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
type workloadEntry struct {
	Namespace string
	Name      string
	Template  *corev1.PodTemplateSpec
	OwnerRefs []metav1.OwnerReference
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
			out[i] = workloadEntry{Namespace: d.Namespace, Name: d.Name, Template: &d.Spec.Template, OwnerRefs: d.OwnerReferences}
		}
		return out, nil
	case "StatefulSet":
		var list appsv1.StatefulSetList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			st := &list.Items[i]
			out[i] = workloadEntry{Namespace: st.Namespace, Name: st.Name, Template: &st.Spec.Template, OwnerRefs: st.OwnerReferences}
		}
		return out, nil
	case "DaemonSet":
		var list appsv1.DaemonSetList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			ds := &list.Items[i]
			out[i] = workloadEntry{Namespace: ds.Namespace, Name: ds.Name, Template: &ds.Spec.Template, OwnerRefs: ds.OwnerReferences}
		}
		return out, nil
	case "CronJob":
		var list batchv1.CronJobList
		if err := s.K8sClient.List(ctx, &list, opts...); err != nil {
			return nil, err
		}
		out := make([]workloadEntry, len(list.Items))
		for i := range list.Items {
			cj := &list.Items[i]
			out[i] = workloadEntry{Namespace: cj.Namespace, Name: cj.Name, Template: &cj.Spec.JobTemplate.Spec.Template, OwnerRefs: cj.OwnerReferences}
		}
		return out, nil
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
			out = append(out, workloadEntry{Namespace: j.Namespace, Name: j.Name, Template: &j.Spec.Template, OwnerRefs: j.OwnerReferences})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

// getWorkloadEntry fetches a single workload by name and returns it as a
// workloadEntry. Used wherever a handler needs the pod template, container
// list, or policy annotation for one specific object.
func (s *Server) getWorkloadEntry(ctx context.Context, namespace, kind, name string) (workloadEntry, error) {
	var obj client.Object
	switch kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "StatefulSet":
		obj = &appsv1.StatefulSet{}
	case "DaemonSet":
		obj = &appsv1.DaemonSet{}
	case "CronJob":
		obj = &batchv1.CronJob{}
	case "Job":
		obj = &batchv1.Job{}
	default:
		return workloadEntry{}, fmt.Errorf("unsupported kind %q", kind)
	}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return workloadEntry{}, err
	}
	tmpl, refs, _, _ := workload.PodTemplateOf(obj)
	return workloadEntry{Namespace: obj.GetNamespace(), Name: obj.GetName(), Template: tmpl, OwnerRefs: refs}, nil
}

// workloadKey assembles the "namespace|kind|name" key used to address a
// workload in Prometheus signal maps. Identically-named workloads in different
// namespaces stay distinct.
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

// fetchWorkloadSignals batches the four signal queries (oom, drift, blocked,
// autoscaler) for every workload at once, then fetches per-workload
// coordination factors only when an autoscaler is present. Returns a map
// keyed by workloadKey(namespace, kind, name) covering every requested key.
func (s *Server) fetchWorkloadSignals(ctx context.Context, keys []string) map[string]workloadSignals {
	if len(keys) == 0 {
		return nil
	}
	oom, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricWorkloadOOM24h, "namespace", "owner_kind", "owner_name")
	drift, _ := s.PromClient.QueryByLabels(ctx, fmt.Sprintf("max by (namespace, owner_kind, owner_name) (abs(1 - %s))", promclient.MetricWorkloadDriftRatio), "namespace", "owner_kind", "owner_name")
	blocked, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricWorkloadRetryState+" == 1", "namespace", "owner_kind", "owner_name")
	autoscaler, _ := s.PromClient.QueryByLabels(ctx, promclient.MetricAutoscalerPresent, "namespace", "owner_kind", "owner_name")

	out := make(map[string]workloadSignals, len(keys))
	for _, key := range keys {
		ns, kind, name := parseWorkloadKey(key)
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
			sig.CoordinationFactors = s.fetchCoordinationFactors(ctx, ns, kind, name)
		}
		out[key] = sig
	}
	return out
}

// parseWorkloadKey is the inverse of workloadKey. The three components never
// contain the "|" separator (Kubernetes name validation forbids it).
func parseWorkloadKey(key string) (namespace, kind, name string) {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 {
		return
	}
	return parts[0], parts[1], parts[2]
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
