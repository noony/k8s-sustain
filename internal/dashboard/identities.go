package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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

// identities takes an inventory snapshot through the dashboard's client, so
// every view sees the identities the controller governs by the same rules.
// The client is uncached: callers narrow opts to what the view shows.
func (s *Server) identities(ctx context.Context, opts inventory.Options) (*inventory.Snapshot, error) {
	opts.ExcludedNamespaces = s.ExcludedNamespaces
	return inventory.Take(ctx, s.K8sClient, opts)
}

// identity reads one identity, narrowing the snapshot to its namespace and
// kind. A key that names no identity is NotFound.
func (s *Server) identity(ctx context.Context, key promclient.WorkloadIdentity) (*inventory.Identity, error) {
	notFound := apierrors.NewNotFound(workload.GroupResourceForKind(key.OwnerKind), key.OwnerName)
	if !slices.Contains(supportedWorkloadKinds, key.OwnerKind) {
		return nil, notFound
	}
	snap, err := s.identities(ctx, inventory.Options{Namespaces: []string{key.Namespace}, Kinds: []string{key.OwnerKind}})
	if err != nil {
		return nil, err
	}
	id, ok := snap.Lookup(key)
	if !ok {
		return nil, notFound
	}
	return id, nil
}

// governingPolicy reads the Policy governing id, or nil when none does or it
// cannot be read.
func (s *Server) governingPolicy(ctx context.Context, id *inventory.Identity) *sustainv1alpha1.Policy {
	if id == nil || id.Policy == "" {
		return nil
	}
	policy := &sustainv1alpha1.Policy{}
	if err := s.K8sClient.Get(ctx, client.ObjectKey{Name: id.Policy}, policy); err != nil {
		s.Logger.V(1).Info("reading the governing policy failed", "policy", id.Policy, "error", err.Error())
		return nil
	}
	return policy
}

// managedKinds returns the kinds policy manages, which bound the identities it
// can govern.
func managedKinds(policy *sustainv1alpha1.Policy) []string {
	var kinds []string
	for _, kind := range supportedWorkloadKinds {
		if policy.Spec.RightSizing.Update.Types.ModeForKind(kind) != nil {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// conflictingPolicies names the Policies a Conflicted identity's members opt
// into, nil otherwise.
func conflictingPolicies(id *inventory.Identity) []string {
	if !id.Conflicted {
		return nil
	}
	return id.MemberPolicies()
}

// workloadKey builds the "namespace|kind|name" key Prometheus label maps are
// matched on.
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

// writeIdentityError answers a failed identity read: 404 for an unknown
// identity, 500 otherwise.
func writeIdentityError(w http.ResponseWriter, key promclient.WorkloadIdentity, err error) {
	writeK8sGetError(w, err, fmt.Sprintf("workload %s/%s/%s: %v", key.Namespace, key.OwnerKind, key.OwnerName, err))
}
