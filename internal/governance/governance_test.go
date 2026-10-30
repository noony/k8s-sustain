package governance_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

var errBoom = errors.New("boom")

// source is a map-backed Reader that records each read, so a row can assert
// which levels the walk paid for.
type source struct {
	policies  []sustainv1alpha1.Policy
	excluded  []string
	workload  map[string]string
	namespace map[string]string
	fail      string
	reads     []string
}

func (s *source) read(what string) error {
	s.reads = append(s.reads, what)
	if s.fail == what {
		return errBoom
	}
	return nil
}

func (s *source) Policies(context.Context) (*governance.Policies, error) {
	if err := s.read("policies"); err != nil {
		return nil, err
	}
	return governance.NewPolicies(s.policies, s.excluded), nil
}

func (s *source) PodOwner(context.Context, *corev1.Pod) (string, string, error) {
	if err := s.read("owner"); err != nil {
		return "", "", err
	}
	return "Deployment", "web", nil
}

func (s *source) WorkloadAnnotations(_ context.Context, _, kind, name string) (map[string]string, error) {
	if err := s.read("workload"); err != nil {
		return nil, err
	}
	if kind != "Deployment" || name != "web" {
		return nil, nil
	}
	return s.workload, nil
}

func (s *source) NamespaceAnnotations(context.Context, string) (map[string]string, error) {
	if err := s.read("namespace"); err != nil {
		return nil, err
	}
	return s.namespace, nil
}

func fixturePolicies() []sustainv1alpha1.Policy {
	mode, onCreate := sustainv1alpha1.UpdateModeOngoing, sustainv1alpha1.UpdateModeOnCreate
	every := sustainv1alpha1.UpdateTypes{
		Deployment: &mode, StatefulSet: &mode, DaemonSet: &mode, ArgoRollout: &mode,
		CronJob: &mode, Job: &mode, Pod: &mode,
	}
	policy := func(name string, types sustainv1alpha1.UpdateTypes, selector sustainv1alpha1.PolicySelector) sustainv1alpha1.Policy {
		return sustainv1alpha1.Policy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: sustainv1alpha1.PolicySpec{
				Selector:    selector,
				RightSizing: sustainv1alpha1.RightSizingSpec{Update: sustainv1alpha1.UpdateSpec{Types: types}},
			},
		}
	}
	return []sustainv1alpha1.Policy{
		policy("p", every, sustainv1alpha1.PolicySelector{}),
		policy("other", every, sustainv1alpha1.PolicySelector{}),
		policy("web-only", every, sustainv1alpha1.PolicySelector{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "web"}},
		}),
		policy("elsewhere", every, sustainv1alpha1.PolicySelector{Namespaces: []string{"other-ns"}}),
		policy("jobs-only", sustainv1alpha1.UpdateTypes{Job: &onCreate}, sustainv1alpha1.PolicySelector{}),
		policy("broken", every, sustainv1alpha1.PolicySelector{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "BogusOp"}}},
		}),
	}
}

type subjectKind int

const (
	template subjectKind = iota
	pod
	barePod
	departed
)

// TestDecide is the governance contract every reader shares: the inventory,
// the webhook and the OOM watcher only adapt their reads to it.
func TestDecide(t *testing.T) {
	pol := func(name string) map[string]string { return map[string]string{sustainv1alpha1.PolicyAnnotation: name} }
	out := map[string]string{sustainv1alpha1.OptOutAnnotation: "true"}
	web := promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Deployment", OwnerName: "web"}
	tierWeb := map[string]string{"tier": "web"}

	cases := []struct {
		name    string
		subject subjectKind
		ns      string
		// tmpl, workload and namespace are the three opt-in levels; labels are
		// the pod template's, objLabels the workload object's own.
		tmpl, workload, namespace map[string]string
		labels, objLabels         map[string]string
		ownerName                 string
		policies                  []string
		departedPolicy            string
		departedKind              string
		fail                      string

		governs string
		// mode is the governing Policy's mode, Ongoing unless set.
		mode     sustainv1alpha1.UpdateMode
		refusal  governance.Refusal
		optIn    string
		level    governance.Level
		identity promclient.WorkloadIdentity
		reads    []string
		wantErr  bool
	}{
		{name: "pod template", tmpl: pol("p"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "workload metadata", workload: pol("p"), governs: "p", optIn: "p", level: governance.LevelWorkload, identity: web, reads: []string{"policies"}},
		{name: "namespace", namespace: pol("p"), governs: "p", optIn: "p", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "namespace"}},
		{name: "workload beats namespace", workload: pol("p"), namespace: pol("other"), governs: "p", optIn: "p", level: governance.LevelWorkload, identity: web, reads: []string{"policies"}},
		{name: "template beats both", tmpl: pol("p"), workload: pol("other"), namespace: pol("other"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "workload opt-out beats namespace opt-in", workload: out, namespace: pol("p"), refusal: governance.OptedOut, identity: web},
		{name: "nothing anywhere", refusal: governance.NotOptedIn, identity: web, reads: []string{"policies", "namespace"}},
		{name: "template opt-out reads nothing", tmpl: out, workload: pol("p"), namespace: pol("p"), refusal: governance.OptedOut, identity: web},
		{name: "namespace opt-out", namespace: out, refusal: governance.OptedOut, identity: web, reads: []string{"policies", "namespace"}},
		{name: "template policy beats workload opt-out", tmpl: pol("p"), workload: out, governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "opt-out beats a policy at the same level", tmpl: map[string]string{sustainv1alpha1.OptOutAnnotation: "true", sustainv1alpha1.PolicyAnnotation: "p"}, refusal: governance.OptedOut, identity: web},
		{name: "only a literal true opts out", workload: map[string]string{sustainv1alpha1.OptOutAnnotation: "yes", sustainv1alpha1.PolicyAnnotation: "p"}, governs: "p", optIn: "p", level: governance.LevelWorkload, identity: web, reads: []string{"policies"}},
		{name: "an empty policy value falls through", tmpl: pol(""), namespace: pol("p"), governs: "p", optIn: "p", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "namespace"}},

		{name: "policy missing", tmpl: pol("ghost"), refusal: governance.PolicyMissing, optIn: "ghost", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "kind not managed", tmpl: pol("jobs-only"), refusal: governance.KindNotManaged, optIn: "jobs-only", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "namespace not selected", tmpl: pol("elsewhere"), refusal: governance.NamespaceNotSelected, optIn: "elsewhere", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "excluded namespace", ns: "kube-system", tmpl: pol("p"), refusal: governance.NamespaceExcluded, optIn: "p", level: governance.LevelPodTemplate, identity: promclient.WorkloadIdentity{Namespace: "kube-system", OwnerKind: "Deployment", OwnerName: "web"}, reads: []string{"policies"}},
		{name: "selector matches pod-template labels", tmpl: pol("web-only"), labels: tierWeb, governs: "web-only", optIn: "web-only", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "selector ignores the workload's own labels", tmpl: pol("web-only"), objLabels: tierWeb, refusal: governance.LabelsNotSelected, optIn: "web-only", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "invalid selector", tmpl: pol("broken"), labels: tierWeb, refusal: governance.SelectorInvalid, optIn: "broken", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "invalid selector named by the namespace", policies: []string{"broken"}, namespace: pol("broken"), refusal: governance.SelectorInvalid, optIn: "broken", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "namespace"}},
		{name: "pod: invalid selector named by the workload", subject: pod, policies: []string{"broken"}, workload: pol("broken"), refusal: governance.SelectorInvalid, optIn: "broken", level: governance.LevelWorkload, identity: web, reads: []string{"policies", "owner", "workload"}},
		{name: "pod: invalid selector named by the namespace", subject: pod, policies: []string{"broken"}, namespace: pol("broken"), refusal: governance.SelectorInvalid, optIn: "broken", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "owner", "workload", "namespace"}},

		{name: "pre-gate: a pod no Policy selects reads nothing more", subject: pod, policies: []string{"web-only"}, namespace: pol("web-only"), refusal: governance.NoPolicySelects, reads: []string{"policies"}},
		{name: "pre-gate: excluded namespace", subject: pod, ns: "kube-system", namespace: pol("p"), refusal: governance.NamespaceExcluded, reads: []string{"policies"}},
		{name: "pre-gate: a template no Policy selects skips the Namespace", policies: []string{"web-only"}, namespace: pol("web-only"), refusal: governance.NoPolicySelects, identity: web, reads: []string{"policies"}},
		{name: "pre-gate passes on pod-template labels", subject: pod, policies: []string{"web-only"}, labels: tierWeb, namespace: pol("web-only"), governs: "web-only", optIn: "web-only", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "owner", "workload", "namespace"}},

		{name: "pod: the template decides before the owner is read", subject: pod, tmpl: pol("p"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies", "owner"}},
		{name: "pod: a refused opt-in never reads the owner", subject: pod, tmpl: pol("ghost"), refusal: governance.PolicyMissing, optIn: "ghost", level: governance.LevelPodTemplate, reads: []string{"policies"}},
		{name: "pod: opt-out reads nothing", subject: pod, tmpl: out, namespace: pol("p"), refusal: governance.OptedOut},
		{name: "pod: the workload level skips the Namespace", subject: pod, workload: pol("p"), governs: "p", optIn: "p", level: governance.LevelWorkload, identity: web, reads: []string{"policies", "owner", "workload"}},
		{name: "pod: namespace level", subject: pod, namespace: pol("p"), governs: "p", optIn: "p", level: governance.LevelNamespace, identity: web, reads: []string{"policies", "owner", "workload", "namespace"}},
		{name: "pod: owner-name names the identity, the real owner the workload level", subject: pod, ownerName: "api", workload: pol("p"), governs: "p", optIn: "p", level: governance.LevelWorkload, identity: promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Deployment", OwnerName: "api"}, reads: []string{"policies", "owner", "workload"}},
		{name: "template: owner-name names the identity", ownerName: "api", tmpl: pol("p"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Deployment", OwnerName: "api"}, reads: []string{"policies"}},
		{name: "an invalid owner-name is ignored", ownerName: "Not/Valid", tmpl: pol("p"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: web, reads: []string{"policies"}},
		{name: "bare pod: owner-name makes a Pod identity", subject: barePod, ownerName: "etl", tmpl: pol("p"), governs: "p", optIn: "p", level: governance.LevelPodTemplate, identity: promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Pod", OwnerName: "etl"}, reads: []string{"policies"}},
		{name: "bare pod: no workload level", subject: barePod, ownerName: "etl", namespace: pol("p"), governs: "p", optIn: "p", level: governance.LevelNamespace, identity: promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Pod", OwnerName: "etl"}, reads: []string{"policies", "namespace"}},
		{name: "bare pod without owner-name", subject: barePod, tmpl: pol("p"), refusal: governance.NoOwner},
		{name: "bare pod with an invalid owner-name", subject: barePod, ownerName: "Not/Valid", tmpl: pol("p"), refusal: governance.NoOwner},

		{name: "departed: governed by its WorkloadRecommendation's Policy", subject: departed, departedPolicy: "p", governs: "p", identity: web, reads: []string{"policies"}},
		{name: "departed: no labels to select", subject: departed, departedPolicy: "web-only", governs: "web-only", identity: web, reads: []string{"policies"}},
		{name: "departed: invalid selector", subject: departed, departedPolicy: "broken", refusal: governance.SelectorInvalid, identity: web, reads: []string{"policies"}},
		{name: "departed: namespace not selected", subject: departed, departedPolicy: "elsewhere", refusal: governance.NamespaceNotSelected, identity: web, reads: []string{"policies"}},
		{name: "departed: excluded namespace", subject: departed, ns: "kube-system", departedPolicy: "p", refusal: governance.NamespaceExcluded, identity: promclient.WorkloadIdentity{Namespace: "kube-system", OwnerKind: "Deployment", OwnerName: "web"}, reads: []string{"policies"}},
		{name: "departed: policy missing", subject: departed, departedPolicy: "ghost", refusal: governance.PolicyMissing, identity: web, reads: []string{"policies"}},
		{name: "departed: kind not managed", subject: departed, departedPolicy: "jobs-only", refusal: governance.KindNotManaged, identity: web, reads: []string{"policies"}},
		{name: "departed: a managed kind", subject: departed, departedKind: "Job", departedPolicy: "jobs-only", governs: "jobs-only", mode: sustainv1alpha1.UpdateModeOnCreate, identity: promclient.WorkloadIdentity{Namespace: "team-a", OwnerKind: "Job", OwnerName: "web"}, reads: []string{"policies"}},

		{name: "error: Policies", tmpl: pol("p"), fail: "policies", wantErr: true},
		{name: "error: owner on the workload level", subject: pod, namespace: pol("p"), fail: "owner", wantErr: true},
		{name: "error: owner after consent", subject: pod, tmpl: pol("p"), fail: "owner", wantErr: true},
		{name: "error: workload", subject: pod, namespace: pol("p"), fail: "workload", wantErr: true},
		{name: "error: namespace", namespace: pol("p"), fail: "namespace", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := tc.ns
			if ns == "" {
				ns = "team-a"
			}
			src := &source{excluded: []string{"kube-system"}, workload: tc.workload, namespace: tc.namespace, fail: tc.fail}
			for _, p := range fixturePolicies() {
				if tc.policies == nil || slices.Contains(tc.policies, p.Name) {
					src.policies = append(src.policies, p)
				}
			}
			annotations := maps.Clone(tc.tmpl)
			if tc.ownerName != "" {
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations[sustainv1alpha1.OwnerNameAnnotation] = tc.ownerName
			}

			var subject governance.Subject
			switch tc.subject {
			case template:
				d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web", Annotations: tc.workload, Labels: tc.objLabels}}
				d.Spec.Template.Labels, d.Spec.Template.Annotations = tc.labels, annotations
				subject = governance.PodTemplate(d, "Deployment", &d.Spec.Template)
			case pod, barePod:
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-abc-1", Labels: tc.labels, Annotations: annotations}}
				if tc.subject == pod {
					p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc", Controller: new(true)}}
				}
				subject = governance.Pod(p)
			case departed:
				kind := tc.departedKind
				if kind == "" {
					kind = "Deployment"
				}
				subject = governance.Departed(promclient.WorkloadIdentity{Namespace: ns, OwnerKind: kind, OwnerName: "web"}, tc.departedPolicy)
			}

			d, err := governance.Decide(context.Background(), subject, src)
			if tc.wantErr {
				if !errors.Is(err, errBoom) {
					t.Fatalf("err = %v, want the read error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if d.PolicyName() != tc.governs || d.Governed() != (tc.governs != "") || d.Refusal != tc.refusal {
				t.Errorf("governs/refusal = %q/%q, want %q/%q", d.PolicyName(), d.Refusal, tc.governs, tc.refusal)
			}
			wantMode := tc.mode
			if wantMode == "" && tc.governs != "" {
				wantMode = sustainv1alpha1.UpdateModeOngoing
			}
			if d.Mode != wantMode {
				t.Errorf("mode = %q, want %q", d.Mode, wantMode)
			}
			if d.OptIn != tc.optIn || d.Level != tc.level {
				t.Errorf("opt-in = %q at %q, want %q at %q", d.OptIn, d.Level, tc.optIn, tc.level)
			}
			if d.Identity != tc.identity {
				t.Errorf("identity = %+v, want %+v", d.Identity, tc.identity)
			}
			if !slices.Equal(src.reads, tc.reads) {
				t.Errorf("reads = %v, want %v", src.reads, tc.reads)
			}
		})
	}
}
