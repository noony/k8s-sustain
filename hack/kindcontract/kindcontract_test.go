package kindcontract

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/hack/recordingrules"
	"github.com/noony/k8s-sustain/internal/config"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

const (
	repoRoot   = "../.."
	typesFile  = "api/v1alpha1/policy_types.go"
	rbacFile   = "charts/k8s-sustain/templates/rbac.yaml"
	valuesFile = recordingrules.ValuesFile
	// ownerKindRule has no constant in internal/prometheus: no Go code
	// queries it.
	ownerKindRule = "k8s_sustain:pod_owner_kind_internal"
)

// roleNames name the chart's ClusterRoles by the template helper each
// metadata.name includes: the controller's, which the webhook shares, and
// the dashboard's.
var roleNames = []string{"k8s-sustain.fullname", "k8s-sustain.dashboardName"}

// places holds what the checks read that is not a Go function they call:
// the kinds, the tables and scheme the binaries use, and the chart.
type places struct {
	kinds      []string
	ownerKinds []string
	disableFor []client.Object
	scheme     *runtime.Scheme
	roles      map[string][]rbacv1.PolicyRule
	rules      map[string][]recordingrules.Rule
}

func loadPlaces(t *testing.T) places {
	t.Helper()
	rules, err := recordingrules.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return places{
		kinds:      slices.Clone(workload.SupportedKinds),
		ownerKinds: workload.OwnerChainKinds(),
		disableFor: k8sclient.OwnerChainDisableFor(),
		scheme:     config.Scheme(),
		roles:      loadRoles(t),
		rules:      rules,
	}
}

var (
	templateOnlyLine  = regexp.MustCompile(`(?m)^[ \t]*\{\{[^\n]*\}\}[ \t]*\n`)
	includeAction     = regexp.MustCompile(`\{\{-?\s*include\s+"([^"]+)"[^}]*\}\}`)
	templateAction    = regexp.MustCompile(`\{\{[^}]*\}\}`)
	documentSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)
)

// loadRoles reads the chart's ClusterRoles from the template itself, keyed
// by the helper naming each: CI's Go test job has no helm to render it.
func loadRoles(t *testing.T) map[string][]rbacv1.PolicyRule {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rbacFile))
	if err != nil {
		t.Fatal(err)
	}
	src := templateOnlyLine.ReplaceAllString(string(b), "")
	src = includeAction.ReplaceAllString(src, "$1")
	src = templateAction.ReplaceAllString(src, "x")
	roles := map[string][]rbacv1.PolicyRule{}
	for _, doc := range documentSeparator.Split(src, -1) {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("%s: %v", rbacFile, err)
		}
		if obj.Kind == "ClusterRole" {
			roles[obj.Metadata.Name] = obj.Rules
		}
	}
	return roles
}

// barePodKind is the kind of the identity a pod without a controller forms:
// it has no owner object and no pod template.
func barePodKind() string {
	kind, _ := workload.ApplyOwnerNameOverride("", "", map[string]string{sustainv1alpha1.OwnerNameAnnotation: "x"})
	return kind
}

// readKinds lists every kind the binaries read objects of: the supported
// kinds and the owner-chain kinds.
func readKinds(p places) []string {
	kinds := slices.Concat(p.kinds, p.ownerKinds)
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// updateFields lists, by JSON name, every UpdateTypes field holding an
// update mode.
func updateFields() []string {
	typ := reflect.TypeFor[sustainv1alpha1.UpdateTypes]()
	var fields []string
	for i := range typ.NumField() {
		if typ.Field(i).Type == reflect.TypeFor[*sustainv1alpha1.UpdateMode]() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			fields = append(fields, name)
		}
	}
	return fields
}

// checkUpdateModes: a kind without a ModeForKind case is never listed by
// inventory.TakeForPolicy, so no Policy can manage it.
func checkUpdateModes(p places) []string {
	var problems []string
	managedBy := map[string]string{}
	for _, kind := range p.kinds {
		field := sustainv1alpha1.UpdateTypesField(kind)
		other, taken := managedBy[field]
		switch {
		case field == "":
			problems = append(problems, fmt.Sprintf("no Policy update mode manages %s: add a *UpdateMode field to UpdateTypes "+
				"and a case returning it to UpdateTypes.ModeForKind in %s, then run make generate manifests", kind, typesFile))
		case taken:
			problems = append(problems, fmt.Sprintf("UpdateTypes.ModeForKind in %s reads field %q for both %s and %s: "+
				"give each kind its own field", typesFile, field, other, kind))
		default:
			managedBy[field] = kind
		}
	}
	for _, field := range updateFields() {
		if _, ok := managedBy[field]; !ok {
			problems = append(problems, fmt.Sprintf("UpdateTypes field %q in %s manages no kind of workload.SupportedKinds: "+
				"add its kind to SupportedKinds in internal/workload/kindobjects.go, or delete the field", field, typesFile))
		}
	}
	return problems
}

// checkObjects: a kind the inventory cannot list, or whose objects yield no
// pod template, has no members, so no Policy ever governs one.
func checkObjects(p places) []string {
	var problems []string
	for _, kind := range p.kinds {
		if kind == barePodKind() {
			continue
		}
		obj := workload.ObjectForKind(kind)
		if obj == nil || workload.ListForKind(kind) == nil {
			problems = append(problems, fmt.Sprintf("%s is not in the ownerKinds table of internal/workload/kindobjects.go: "+
				"add its object and list constructors and its GroupResource", kind))
			continue
		}
		if _, ok := workload.PodTemplateOf(obj); !ok {
			problems = append(problems, fmt.Sprintf("workload.PodTemplateOf returns no pod template for a %s, so the inventory "+
				"skips every one: add a case to internal/workload/templates.go", kind))
		}
	}
	return problems
}

// checkOwnerChainReads: the webhook reads an owner-chain kind at most once
// per pod CREATE, so an informer over every object of the kind would cost
// more than it saves, and its LIST, charged to the admission's governance
// timeout, would time out on a large cluster and fail open.
func checkOwnerChainReads(p places) []string {
	disabled := map[reflect.Type]bool{}
	for _, obj := range p.disableFor {
		disabled[reflect.TypeOf(obj)] = true
	}
	var problems []string
	for _, kind := range p.ownerKinds {
		if typ := reflect.TypeOf(workload.ObjectForKind(kind)); !disabled[typ] {
			problems = append(problems, fmt.Sprintf("k8s.OwnerChainDisableFor in internal/k8s/client.go does not list %s (%s): "+
				"the webhook's first Get of one would stand up a cluster-wide informer on the admission path", kind, typ))
		}
	}
	return problems
}

// checkScheme: every binary reads through config.Scheme, and a client
// cannot read a kind its scheme does not know.
func checkScheme(p places) []string {
	var problems []string
	for _, kind := range readKinds(p) {
		group := workload.GroupResourceForKind(kind).Group
		if !knows(p.scheme, group, kind) {
			problems = append(problems, fmt.Sprintf("config.Scheme in internal/config/config.go does not know kind %s of API group %q: "+
				"add the group's AddToScheme", kind, group))
		}
	}
	return problems
}

// knows reports whether scheme maps group and kind to a Go type, the one
// workload.ObjectForKind builds when it builds one.
func knows(scheme *runtime.Scheme, group, kind string) bool {
	match := func(gvk schema.GroupVersionKind) bool { return gvk.Group == group && gvk.Kind == kind }
	if obj := workload.ObjectForKind(kind); obj != nil {
		gvks, _, err := scheme.ObjectKinds(obj)
		return err == nil && slices.ContainsFunc(gvks, match)
	}
	return slices.ContainsFunc(slices.Collect(maps.Keys(scheme.AllKnownTypes())), match)
}

// checkRBAC: the chart's ClusterRoles are the only RBAC there is.
func checkRBAC(p places) []string {
	var problems []string
	for _, role := range roleNames {
		rules, ok := p.roles[role]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s has no ClusterRole named by %s", rbacFile, role))
			continue
		}
		for _, kind := range readKinds(p) {
			gr := workload.GroupResourceForKind(kind)
			var missing []string
			for _, verb := range []string{"get", "list", "watch"} {
				if !grants(rules, gr, verb) {
					missing = append(missing, verb)
				}
			}
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("the ClusterRole named by %s in %s does not grant %s on %s, "+
					"which %s objects are read through: add them to its rules", role, rbacFile, strings.Join(missing, ", "), gr, kind))
			}
		}
	}
	return problems
}

func grants(rules []rbacv1.PolicyRule, gr schema.GroupResource, verb string) bool {
	has := func(values []string, v string) bool {
		return slices.Contains(values, v) || slices.Contains(values, "*")
	}
	return slices.ContainsFunc(rules, func(r rbacv1.PolicyRule) bool {
		return has(r.APIGroups, gr.Group) && has(r.Resources, gr.Resource) && has(r.Verbs, verb)
	})
}

// resolvedThrough maps each kind workload.ResolveControllerOwner reads a
// pod's controller through to the supported kinds it resolves it to: an
// owner-chain kind owned by a supported kind resolves to that kind only when
// the walk goes through it.
func resolvedThrough(p places) (map[string][]string, []string) {
	through := map[string][]string{}
	var problems []string
	for _, middle := range p.ownerKinds {
		for _, kind := range p.kinds {
			owner := workload.ObjectForKind(middle)
			owner.SetNamespace("ns")
			owner.SetName("middle")
			owner.SetOwnerReferences([]metav1.OwnerReference{{Kind: kind, Name: "top", Controller: ptr.To(true)}})
			// Its own scheme, so a kind config.Scheme misses fails the scheme
			// check alone.
			s := runtime.NewScheme()
			s.AddKnownTypes(schema.GroupVersion{Group: workload.GroupResourceForKind(middle).Group, Version: "v1"}, owner)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
			gotKind, gotName, err := workload.ResolveControllerOwner(context.Background(), c, "ns",
				metav1.OwnerReference{Kind: middle, Name: "middle", Controller: ptr.To(true)})
			if err != nil {
				problems = append(problems, fmt.Sprintf("resolving a pod of a %s owned by a %s: %v", middle, kind, err))
				continue
			}
			if gotKind == kind && gotName == "top" {
				through[middle] = append(through[middle], kind)
			}
		}
	}
	for middle := range through {
		slices.Sort(through[middle])
	}
	return through, problems
}

var (
	ownerSeries       = regexp.MustCompile(`\b(kube_[a-z]+_owner)\s*\{([^}]*)\}`)
	ownerKindMatchers = regexp.MustCompile(`\bowner_kind\s*(!=|!~|=~|=)\s*"([^"]*)"`)
	kindAlternation   = regexp.MustCompile(`^[A-Za-z0-9]+(\|[A-Za-z0-9]+)*$`)
)

// selection is one selector of a kube-state-metrics owner series: the owner
// kinds it keeps, every one when unfiltered.
type selection struct {
	series     string
	kinds      []string
	unfiltered bool
}

// selections reads expr's owner-series selectors, reporting any whose
// owner_kind matcher is not a literal or an alternation of literals.
func selections(rule, expr string) ([]selection, []string) {
	var sels []selection
	var problems []string
	for _, m := range ownerSeries.FindAllStringSubmatch(expr, -1) {
		matchers := ownerKindMatchers.FindAllStringSubmatch(m[2], -1)
		switch {
		case len(matchers) == 0:
			sels = append(sels, selection{series: m[1], unfiltered: true})
		case len(matchers) == 1 && (matchers[0][1] == "=" || matchers[0][1] == "=~") && kindAlternation.MatchString(matchers[0][2]):
			sels = append(sels, selection{series: m[1], kinds: strings.Split(matchers[0][2], "|")})
		default:
			problems = append(problems, fmt.Sprintf("%s selects %s{%s}, whose owner_kind matcher the kind contract cannot read: "+
				`use owner_kind="Kind" or owner_kind=~"KindA|KindB" in %s`, rule, m[1], strings.TrimSpace(m[2]), valuesFile))
		}
	}
	return sels, problems
}

func kindsSelected(sels []selection, keep func(selection) bool) []string {
	var kinds []string
	for _, s := range sels {
		if keep(s) {
			kinds = append(kinds, s.kinds...)
		}
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// checkRecordingRules: a kind whose pods the rules map to no identity, or to
// another one than the controller does, gets no usage, hence NoData forever.
// Both k8s_sustain:pod_workload and k8s_sustain:pod_owner_kind_internal must
// walk a pod's controller through exactly the kinds ResolveControllerOwner
// walks through, to exactly the kinds it resolves; pod_workload maps every
// other supported kind straight from the pod's controller, and
// pod_owner_kind_internal every other controller kind, as the controller's
// default does.
func checkRecordingRules(p places) []string {
	through, problems := resolvedThrough(p)
	indirect := map[string]bool{barePodKind(): true}
	for _, kinds := range through {
		for _, kind := range kinds {
			indirect[kind] = true
		}
	}
	direct := without(p.kinds, func(kind string) bool { return indirect[kind] })
	slices.Sort(direct)
	isMiddle := func(s selection) bool { return len(s.kinds) == 1 && len(through[s.kinds[0]]) > 0 }
	isPod := func(s selection) bool { return s.series == "kube_pod_owner" }

	for _, rule := range []string{promclient.MetricPodWorkload, ownerKindRule} {
		defs := p.rules[rule]
		if len(defs) == 0 {
			problems = append(problems, fmt.Sprintf("prometheusRule.groups in %s records no %s", valuesFile, rule))
			continue
		}
		var sels []selection
		for _, d := range defs {
			s, unreadable := selections(rule, d.Expr)
			sels = append(sels, s...)
			problems = append(problems, unreadable...)
		}
		for _, middle := range slices.Sorted(maps.Keys(through)) {
			series := "kube_" + strings.ToLower(middle) + "_owner"
			got := kindsSelected(sels, func(s selection) bool { return s.series == series })
			if !slices.Equal(got, through[middle]) {
				problems = append(problems, fmt.Sprintf("%s maps a pod through a %s to owner kinds %q, but workload.ResolveControllerOwner "+
					"resolves a %s to %q: make its %s selectors in %s select exactly those, or change ResolveControllerOwner in "+
					"internal/workload/ownerref.go", rule, middle, got, middle, through[middle], series, valuesFile))
			}
		}
		switch rule {
		case promclient.MetricPodWorkload:
			if slices.ContainsFunc(sels, func(s selection) bool { return isPod(s) && s.unfiltered }) {
				problems = append(problems, fmt.Sprintf("%s selects kube_pod_owner without an owner_kind matcher in %s: "+
					"it would map a pod of any controller kind, supported or not", rule, valuesFile))
			}
			got := kindsSelected(sels, func(s selection) bool { return isPod(s) && !isMiddle(s) })
			if !slices.Equal(got, direct) {
				problems = append(problems, fmt.Sprintf("%s maps a pod straight from its controller for owner kinds %q, but the "+
					"controller does for %q: make the owner_kind regex of the direct-owner kube_pod_owner selector in %s "+
					"select exactly those", rule, got, direct, valuesFile))
			}
		case ownerKindRule:
			if !slices.ContainsFunc(sels, func(s selection) bool { return isPod(s) && s.unfiltered }) {
				problems = append(problems, fmt.Sprintf("%s keeps no kube_pod_owner alternative without an owner_kind matcher, but "+
					"workload.ResolveControllerOwner resolves any other controller to its own kind: restore it in %s", rule, valuesFile))
			}
		}
	}
	return problems
}

var checks = map[string]func(places) []string{
	"update modes":      checkUpdateModes,
	"objects":           checkObjects,
	"owner-chain reads": checkOwnerChainReads,
	"scheme":            checkScheme,
	"RBAC":              checkRBAC,
	"recording rules":   checkRecordingRules,
}

// Every place a workload kind must be registered in agrees with
// workload.SupportedKinds, the one kind table.
func TestEveryPlaceAgreesWithSupportedKinds(t *testing.T) {
	p := loadPlaces(t)
	for _, name := range slices.Sorted(maps.Keys(checks)) {
		for _, problem := range checks[name](p) {
			t.Errorf("%s: %s", name, problem)
		}
	}
}

func without[T any](s []T, drop func(T) bool) []T {
	return slices.DeleteFunc(slices.Clone(s), drop)
}

func editRule(p *places, rule, old, replacement string) {
	defs := slices.Clone(p.rules[rule])
	for i := range defs {
		defs[i].Expr = strings.ReplaceAll(defs[i].Expr, old, replacement)
	}
	p.rules[rule] = defs
}

// Each way a place can disagree with the kinds is reported, naming the
// place to edit, starting from places that agree.
func TestChecksReportEachBreach(t *testing.T) {
	addKind := func(kind string) func(*places) {
		return func(p *places) { p.kinds = append(p.kinds, kind) }
	}
	cases := []struct {
		name  string
		check string
		edit  func(*places)
		want  []string
	}{
		{
			"a kind no update mode manages", "update modes", addKind("Widget"),
			[]string{"no Policy update mode manages Widget: add a *UpdateMode field to UpdateTypes and a case returning it to UpdateTypes.ModeForKind in " + typesFile},
		},
		{
			"an update mode managing no kind", "update modes",
			func(p *places) { p.kinds = without(p.kinds, func(k string) bool { return k == "DaemonSet" }) },
			[]string{`UpdateTypes field "daemonSet" in ` + typesFile + " manages no kind"},
		},
		{
			"a kind outside the ownerKinds table", "objects", addKind("Widget"),
			[]string{"Widget is not in the ownerKinds table of internal/workload/kindobjects.go"},
		},
		{
			"a kind without a pod template", "objects", addKind("ReplicaSet"),
			[]string{"workload.PodTemplateOf returns no pod template for a ReplicaSet, so the inventory skips every one: add a case to internal/workload/templates.go"},
		},
		{
			"an owner-chain kind read through an informer", "owner-chain reads",
			func(p *places) {
				rollout := reflect.TypeOf(workload.ObjectForKind("Rollout"))
				p.disableFor = without(p.disableFor, func(o client.Object) bool { return reflect.TypeOf(o) == rollout })
			},
			[]string{"k8s.OwnerChainDisableFor in internal/k8s/client.go does not list Rollout"},
		},
		{
			"a kind the scheme does not know", "scheme",
			func(p *places) {
				s := runtime.NewScheme()
				utilruntime.Must(clientgoscheme.AddToScheme(s))
				utilruntime.Must(sustainv1alpha1.AddToScheme(s))
				p.scheme = s
			},
			[]string{`config.Scheme in internal/config/config.go does not know kind Rollout of API group "argoproj.io"`},
		},
		{
			"a ClusterRole that cannot read a kind", "RBAC",
			func(p *places) {
				var rules []rbacv1.PolicyRule
				for _, r := range p.roles["k8s-sustain.dashboardName"] {
					r.Resources = without(r.Resources, func(res string) bool { return res == "replicasets" })
					rules = append(rules, r)
				}
				p.roles["k8s-sustain.dashboardName"] = rules
			},
			[]string{"the ClusterRole named by k8s-sustain.dashboardName in " + rbacFile + " does not grant get, list, watch on replicasets.apps"},
		},
		{
			"a missing ClusterRole", "RBAC",
			func(p *places) { delete(p.roles, "k8s-sustain.fullname") },
			[]string{rbacFile + " has no ClusterRole named by k8s-sustain.fullname"},
		},
		{
			"a supported kind the direct-owner rule leaves out", "recording rules", addKind("Widget"),
			[]string{promclient.MetricPodWorkload + ` maps a pod straight from its controller for owner kinds ["DaemonSet" "Job" "StatefulSet"], but the controller does for ["DaemonSet" "Job" "StatefulSet" "Widget"]`},
		},
		{
			"a direct kind dropped from the rule", "recording rules",
			func(p *places) {
				editRule(p, promclient.MetricPodWorkload, `owner_kind=~"StatefulSet|DaemonSet|Job"`, `owner_kind=~"StatefulSet|DaemonSet"`)
			},
			[]string{`for owner kinds ["DaemonSet" "StatefulSet"], but the controller does for ["DaemonSet" "Job" "StatefulSet"]`},
		},
		{
			"a kind resolved through a ReplicaSet left out", "recording rules",
			func(p *places) {
				editRule(p, ownerKindRule, `owner_kind=~"Deployment|Rollout"`, `owner_kind="Deployment"`)
			},
			[]string{ownerKindRule + ` maps a pod through a ReplicaSet to owner kinds ["Deployment"], but workload.ResolveControllerOwner resolves a ReplicaSet to ["Deployment" "Rollout"]`},
		},
		{
			"no fallback to the controller's own kind", "recording rules",
			func(p *places) {
				editRule(p, ownerKindRule, `kube_pod_owner{owner_is_controller="true"}`, `kube_pod_owner{owner_kind="StatefulSet", owner_is_controller="true"}`)
			},
			[]string{ownerKindRule + " keeps no kube_pod_owner alternative without an owner_kind matcher"},
		},
		{
			"an owner_kind matcher the contract cannot read", "recording rules",
			func(p *places) {
				editRule(p, ownerKindRule, `owner_kind=~"Deployment|Rollout"`, `owner_kind=~"Deploy.*|Rollout"`)
			},
			[]string{
				ownerKindRule + " selects kube_replicaset_owner{owner_kind=~\"Deploy.*|Rollout\", owner_is_controller=\"true\"}, whose owner_kind matcher the kind contract cannot read",
				ownerKindRule + " maps a pod through a ReplicaSet to owner kinds []",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := loadPlaces(t)
			if problems := checks[tc.check](p); len(problems) != 0 {
				t.Fatalf("before the edit: %q", problems)
			}
			tc.edit(&p)

			problems := checks[tc.check](p)

			if len(problems) != len(tc.want) {
				t.Fatalf("problems = %q, want %d", problems, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(problems[i], want) {
					t.Errorf("problem %d = %q, want it to contain %q", i, problems[i], want)
				}
			}
		})
	}
}
