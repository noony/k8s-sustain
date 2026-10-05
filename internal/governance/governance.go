// Package governance decides, for one pod or pod template, the identity it
// reports into, the Policy it opts into, and whether that Policy governs it.
// The inventory (and through it the controller and the dashboard), the
// webhook and the OOM watcher all decide through Decide, so they cannot
// disagree on who governs a pod.
package governance

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// Level names the annotation level an opt-in was resolved from. The string
// values are stable and safe to log.
type Level string

const (
	// LevelNone means the subject opted into nothing.
	LevelNone Level = ""
	// LevelPodTemplate is spec.template.metadata.annotations, or a pod's own
	// annotations.
	LevelPodTemplate Level = "podTemplate"
	// LevelWorkload is the workload object's own metadata.annotations.
	LevelWorkload Level = "workload"
	// LevelNamespace is the Namespace object's metadata.annotations.
	LevelNamespace Level = "namespace"
)

// Refusal says why no Policy governs a subject. The string values are stable
// and safe to log.
type Refusal string

const (
	// NoOwner: a pod with no controller and no valid owner-name annotation
	// reports into no identity.
	NoOwner Refusal = "no-owner"
	// NoPolicySelects: no Policy accepts the subject's namespace and labels,
	// so whatever it opts into cannot govern it and its opt-in is not read.
	NoPolicySelects Refusal = "no-policy-selects"
	NotOptedIn      Refusal = "not-opted-in"
	OptedOut        Refusal = "opted-out"
	PolicyMissing   Refusal = "policy-missing"
	// NamespaceExcluded: the operator's --excluded-namespaces names the
	// subject's namespace.
	NamespaceExcluded    Refusal = "namespace-excluded"
	NamespaceNotSelected Refusal = "namespace-not-selected"
	SelectorInvalid      Refusal = "selector-invalid"
	LabelsNotSelected    Refusal = "labels-not-selected"
	KindNotManaged       Refusal = "kind-not-managed"
)

// Identity is the namespace, owner kind and owner name a pod reports into,
// the owner-name override applied.
type Identity struct {
	Namespace string
	OwnerKind string
	OwnerName string
}

// Decision is what Decide found out about one subject.
type Decision struct {
	// Identity is what the subject reports into. It is set for a pod template
	// and a Departed identity, and for a pod once the walk has needed its
	// owner; it is zero for a pod refused before then.
	Identity Identity
	// OptIn is the Policy the subject opts into and Level where; empty when it
	// opts into none or was refused before its opt-in was read.
	OptIn string
	Level Level
	// Policy is the governing Policy, nil when Refusal is set.
	Policy  *sustainv1alpha1.Policy
	Refusal Refusal
}

// Governed reports whether a Policy governs the subject.
func (d Decision) Governed() bool { return d.Policy != nil }

// PolicyName is the governing Policy's name, empty when none governs.
func (d Decision) PolicyName() string {
	if d.Policy == nil {
		return ""
	}
	return d.Policy.Name
}

// Subject is the pod template or pod a Decision is about. Its labels are what
// a Policy's label selector matches, and its annotations are the most
// specific opt-in level.
type Subject struct {
	namespace   string
	labels      map[string]string
	annotations map[string]string
	pod         *corev1.Pod
	workload    *workloadMeta
	departed    *departure
}

type workloadMeta struct {
	kind, name  string
	annotations map[string]string
}

type departure struct {
	identity Identity
	policy   string
}

// PodTemplate is the pod template of a workload object of kind: a
// CronJob's job template, a Deployment's template, and so on.
func PodTemplate(obj metav1.Object, kind string, tmpl *corev1.PodTemplateSpec) Subject {
	return Subject{
		namespace:   obj.GetNamespace(),
		labels:      tmpl.Labels,
		annotations: tmpl.Annotations,
		workload:    &workloadMeta{kind: kind, name: obj.GetName(), annotations: obj.GetAnnotations()},
	}
}

// Pod is a pod being admitted or running. A pod with no controller is its own
// workload: it has no workload level and reports into an identity only
// through a valid owner-name annotation.
func Pod(pod *corev1.Pod) Subject {
	return Subject{namespace: pod.Namespace, labels: pod.Labels, annotations: pod.Annotations, pod: pod}
}

// Departed is an identity with no live member left, which opts into nothing:
// it is governed by the Policy its WorkloadRecommendation names while that
// Policy exists, manages its kind and selects its namespace. It has no labels
// for a label selector to match.
func Departed(id Identity, policy string) Subject {
	return Subject{namespace: id.Namespace, departed: &departure{identity: id, policy: policy}}
}

// Reader is where Decide reads what a subject does not carry. Decide calls a
// method only when its answer can change the Decision, at most once per call,
// and returns any error it gets unchanged.
type Reader interface {
	// Policies returns every Policy and the excluded namespaces.
	Policies(ctx context.Context) (*Policies, error)
	// PodOwner resolves a pod's controller to the top of its chain (a
	// ReplicaSet to its Deployment or Rollout, a Job to its CronJob). It is
	// called only for a pod with a controller.
	PodOwner(ctx context.Context, pod *corev1.Pod) (kind, name string, err error)
	// WorkloadAnnotations returns a workload object's own annotations, nil when
	// it or its kind does not exist. It is called only for a pod.
	WorkloadAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error)
	// NamespaceAnnotations returns a Namespace's annotations, nil when it does
	// not exist.
	NamespaceAnnotations(ctx context.Context, namespace string) (map[string]string, error)
}

// Decide walks the subject's opt-in from its pod template to its workload to
// its Namespace, and decides whether the Policy it opts into governs it: the
// Policy must exist, accept its namespace (and not be in an excluded one),
// select its pod-template labels and manage its kind. Each level is read only
// when every more specific one is silent, and before the first read the walk
// checks that some Policy could select the subject at all.
func Decide(ctx context.Context, s Subject, r Reader) (Decision, error) {
	w := &walk{ctx: ctx, s: s, r: r}
	if s.departed != nil {
		return w.departedDecision()
	}
	switch {
	case s.workload != nil:
		w.ownerKnown, w.ownerKind, w.ownerName = true, s.workload.kind, s.workload.name
	case metav1.GetControllerOf(s.pod) == nil:
		w.ownerKnown = true
		if w.identity().OwnerKind == "" {
			return Decision{Refusal: NoOwner}, nil
		}
	}
	d, err := w.decide()
	if err != nil {
		return Decision{}, err
	}
	if w.ownerKnown {
		d.Identity = w.identity()
	}
	return d, nil
}

type walk struct {
	ctx      context.Context
	s        Subject
	r        Reader
	policies *Policies
	gated    bool

	ownerKnown           bool
	ownerKind, ownerName string
}

func (w *walk) decide() (Decision, error) {
	name, level, refusal, err := w.optIn()
	if err != nil || refusal != "" {
		return Decision{Refusal: refusal}, err
	}
	d := Decision{OptIn: name, Level: level}
	ps, err := w.readPolicies()
	if err != nil {
		return d, err
	}
	p, refusal := ps.consent(name, w.s.namespace, w.s.labels, true)
	if refusal != "" {
		d.Refusal = refusal
		return d, nil
	}
	if err := w.readOwner(); err != nil {
		return d, err
	}
	if p.Spec.RightSizing.Update.Types.ModeForKind(w.identity().OwnerKind) == nil {
		d.Refusal = KindNotManaged
		return d, nil
	}
	d.Policy = p
	return d, nil
}

func (w *walk) departedDecision() (Decision, error) {
	d := Decision{Identity: w.s.departed.identity}
	ps, err := w.readPolicies()
	if err != nil {
		return Decision{}, err
	}
	p, refusal := ps.consent(w.s.departed.policy, w.s.namespace, nil, false)
	if refusal != "" {
		d.Refusal = refusal
		return d, nil
	}
	if p.Spec.RightSizing.Update.Types.ModeForKind(d.Identity.OwnerKind) == nil {
		d.Refusal = KindNotManaged
		return d, nil
	}
	d.Policy = p
	return d, nil
}

// optIn returns the Policy the first deciding level names. A level decides
// when it opts out or names a Policy; an empty name falls through, since that
// is what a Helm template renders for an unset value.
func (w *walk) optIn() (string, Level, Refusal, error) {
	levels := []struct {
		level Level
		read  func() (map[string]string, Refusal, error)
	}{
		{LevelPodTemplate, func() (map[string]string, Refusal, error) { return w.s.annotations, "", nil }},
		{LevelWorkload, w.workloadAnnotations},
		{LevelNamespace, w.namespaceAnnotations},
	}
	for _, l := range levels {
		ann, refusal, err := l.read()
		if err != nil || refusal != "" {
			return "", LevelNone, refusal, err
		}
		if ann[sustainv1alpha1.OptOutAnnotation] == "true" {
			return "", LevelNone, OptedOut, nil
		}
		if name := ann[sustainv1alpha1.PolicyAnnotation]; name != "" {
			return name, l.level, "", nil
		}
	}
	return "", LevelNone, NotOptedIn, nil
}

func (w *walk) workloadAnnotations() (map[string]string, Refusal, error) {
	if w.s.workload != nil {
		return w.s.workload.annotations, "", nil
	}
	if w.ownerKnown && w.ownerKind == "" {
		return nil, "", nil
	}
	if refusal, err := w.gate(); err != nil || refusal != "" {
		return nil, refusal, err
	}
	if err := w.readOwner(); err != nil {
		return nil, "", err
	}
	ann, err := w.r.WorkloadAnnotations(w.ctx, w.s.namespace, w.ownerKind, w.ownerName)
	return ann, "", err
}

func (w *walk) namespaceAnnotations() (map[string]string, Refusal, error) {
	if refusal, err := w.gate(); err != nil || refusal != "" {
		return nil, refusal, err
	}
	ann, err := w.r.NamespaceAnnotations(w.ctx, w.s.namespace)
	return ann, "", err
}

// gate refuses a subject no Policy could select, before the walk pays for a
// read. It is sound because the full decision applies the same namespace and
// label rule to whichever Policy the walk would have found.
func (w *walk) gate() (Refusal, error) {
	if w.gated {
		return "", nil
	}
	ps, err := w.readPolicies()
	if err != nil {
		return "", err
	}
	w.gated = true
	return ps.anySelects(w.s.namespace, w.s.labels), nil
}

func (w *walk) readPolicies() (*Policies, error) {
	if w.policies != nil {
		return w.policies, nil
	}
	ps, err := w.r.Policies(w.ctx)
	if err != nil {
		return nil, err
	}
	w.policies = ps
	return ps, nil
}

func (w *walk) readOwner() error {
	if w.ownerKnown {
		return nil
	}
	kind, name, err := w.r.PodOwner(w.ctx, w.s.pod)
	if err != nil {
		return err
	}
	w.ownerKnown, w.ownerKind, w.ownerName = true, kind, name
	return nil
}

// identity applies the owner-name override to the real owner; the workload
// level is read from the real owner, never from the overridden name.
func (w *walk) identity() Identity {
	kind, name := workload.ApplyOwnerNameOverride(w.ownerKind, w.ownerName, w.s.annotations)
	return Identity{Namespace: w.s.namespace, OwnerKind: kind, OwnerName: name}
}
