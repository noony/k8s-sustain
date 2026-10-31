package workload

import (
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Injection is a Recommendation narrowed to the containers of one pod or pod
// template, with what applying it sets on each.
type Injection struct {
	Recs map[string]ContainerRecommendation
	// Writes lists the containers whose resources change: containers first,
	// then init containers, each in declaration order.
	Writes []ContainerWrite
}

type ContainerWrite struct {
	Name string
	// Init and Index locate the container: spec.initContainers[Index] when
	// Init is set, spec.containers[Index] otherwise.
	Init      bool
	Index     int
	Resources corev1.ResourceRequirements
	// RaisedLimits names each resource whose limit was raised to its request.
	RaisedLimits []corev1.ResourceName
}

// LogRaisedLimits logs at debug the limits Inject raised to their request,
// for whoever writes w.
func (w ContainerWrite) LogRaisedLimits(logger logr.Logger) {
	if len(w.RaisedLimits) > 0 {
		logger.V(1).Info("raised limits to the recommended request", "container", w.Name, "resources", w.RaisedLimits)
	}
}

// Inject narrows recs to the containers declared, and to the init containers
// too unless excludeInit, and returns what to set on each. A container no
// recommendation names is left alone, and a name declared both as a container
// and an init container gets the one recommendation in both places.
//
// It never leaves a request above its limit, which the apiserver rejects on a
// pod and on a resize: when the request a container ends up with exceeds the
// limit it ends up with, the limit is raised to the request. That covers a
// recommended request above a limit the recommendation keeps, and a request
// decrease the downsize tolerance withholds while letting the limit's through.
func Inject(containers, initContainers []corev1.Container, excludeInit bool, recs map[string]ContainerRecommendation) Injection {
	inj := Injection{Recs: make(map[string]ContainerRecommendation, len(recs))}
	add := func(cs []corev1.Container, init bool) {
		for i, c := range cs {
			rec, ok := recs[c.Name]
			if !ok {
				continue
			}
			inj.Recs[c.Name] = rec
			res, raised, changed := applyTo(c.Resources, rec)
			if changed {
				inj.Writes = append(inj.Writes, ContainerWrite{Name: c.Name, Init: init, Index: i, Resources: res, RaisedLimits: raised})
			}
		}
	}
	add(containers, false)
	if !excludeInit {
		add(initContainers, true)
	}
	return inj
}

// applyTo returns current with rec applied and no request above its limit,
// the resources whose limit it raised, and whether any CPU or memory request
// or limit changed in value. An unset value reads as zero.
func applyTo(current corev1.ResourceRequirements, rec ContainerRecommendation) (corev1.ResourceRequirements, []corev1.ResourceName, bool) {
	out := *current.DeepCopy()
	setQuantity(&out.Requests, corev1.ResourceCPU, rec.CPURequest)
	applyLimit(&out.Limits, corev1.ResourceCPU, rec.CPULimit, rec.RemoveCPULimit)
	setQuantity(&out.Requests, corev1.ResourceMemory, rec.MemoryRequest)
	applyLimit(&out.Limits, corev1.ResourceMemory, rec.MemoryLimit, rec.RemoveMemoryLimit)

	var raised []corev1.ResourceName
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		req, hasReq := out.Requests[name]
		limit, hasLimit := out.Limits[name]
		if hasReq && hasLimit && req.Cmp(limit) > 0 {
			out.Limits[name] = req
			raised = append(raised, name)
		}
	}
	return out, raised, !valueEqual(current, out)
}

func valueEqual(a, b corev1.ResourceRequirements) bool {
	return requestsEqual(a, b) &&
		a.Limits.Cpu().Cmp(*b.Limits.Cpu()) == 0 &&
		a.Limits.Memory().Cmp(*b.Limits.Memory()) == 0
}

func requestsEqual(a, b corev1.ResourceRequirements) bool {
	return a.Requests.Cpu().Cmp(*b.Requests.Cpu()) == 0 &&
		a.Requests.Memory().Cmp(*b.Requests.Memory()) == 0
}

// setQuantity sets list[name] = *q. A nil q leaves it alone, and so does a q
// numerically equal to the current value, an absent key comparing as zero.
func setQuantity(list *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity) {
	if q == nil {
		return
	}
	if cur := (*list)[name]; cur.Cmp(*q) == 0 {
		return
	}
	if *list == nil {
		*list = corev1.ResourceList{}
	}
	(*list)[name] = *q
}

// applyLimit deletes the limit when remove is set, otherwise defers to
// setQuantity. remove wins over a non-nil q.
func applyLimit(list *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity, remove bool) {
	if !remove {
		setQuantity(list, name, q)
		return
	}
	delete(*list, name)
}
