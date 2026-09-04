package policymatch

import (
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func ann(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestResolvePolicy(t *testing.T) {
	policy := sustainv1alpha1.PolicyAnnotation
	optOut := sustainv1alpha1.OptOutAnnotation

	cases := []struct {
		name                          string
		template, workload, namespace map[string]string
		wantName                      string
		wantLevel                     Level
	}{
		{"all nil", nil, nil, nil, "", LevelNone},
		{"template only", ann(policy, "p"), nil, nil, "p", LevelPodTemplate},
		{"workload only", nil, ann(policy, "p"), nil, "p", LevelWorkload},
		{"namespace only", nil, nil, ann(policy, "p"), "p", LevelNamespace},
		{"template beats workload", ann(policy, "tpl"), ann(policy, "wl"), nil, "tpl", LevelPodTemplate},
		{"workload beats namespace", nil, ann(policy, "wl"), ann(policy, "ns"), "wl", LevelWorkload},
		{"template beats namespace", ann(policy, "tpl"), nil, ann(policy, "ns"), "tpl", LevelPodTemplate},
		{"template opt-out stops the walk", ann(optOut, "true"), ann(policy, "wl"), ann(policy, "ns"), "", LevelNone},
		{"workload opt-out stops the walk", nil, ann(optOut, "true"), ann(policy, "ns"), "", LevelNone},
		{"namespace opt-out is a no-op", nil, nil, ann(optOut, "true"), "", LevelNone},
		{"template policy beats workload opt-out", ann(policy, "tpl"), ann(optOut, "true"), nil, "tpl", LevelPodTemplate},
		{"opt-out beats policy in the same level", ann(optOut, "true", policy, "p"), nil, nil, "", LevelNone},
		{"opt-out only counts when literally true", nil, ann(optOut, "yes", policy, "wl"), nil, "wl", LevelWorkload},
		{"empty value falls through, is not an opt-out", ann(policy, ""), nil, ann(policy, "ns"), "ns", LevelNamespace},
		{"unrelated annotations ignored", ann("other", "x"), nil, nil, "", LevelNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotLevel := ResolvePolicy(tc.template, tc.workload, tc.namespace)
			if gotName != tc.wantName || gotLevel != tc.wantLevel {
				t.Errorf("ResolvePolicy() = (%q, %q), want (%q, %q)", gotName, gotLevel, tc.wantName, tc.wantLevel)
			}
		})
	}
}

// Callers that skip a Namespace/owner read when DecidesAt is true depend on it
// matching ResolvePolicy's loop body exactly: any divergence either pays for a
// read ResolvePolicy never needed, or skips one it did.
func TestDecidesAt(t *testing.T) {
	policy := sustainv1alpha1.PolicyAnnotation
	optOut := sustainv1alpha1.OptOutAnnotation

	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"nil map", nil, false},
		{"empty map", ann(), false},
		{"unrelated annotation", ann("other", "x"), false},
		{"policy set", ann(policy, "p"), true},
		{"empty policy value falls through", ann(policy, ""), false},
		{"opt-out true", ann(optOut, "true"), true},
		{"opt-out not literally true", ann(optOut, "yes"), false},
		{"opt-out true and policy set", ann(optOut, "true", policy, "p"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecidesAt(tc.annotations); got != tc.want {
				t.Errorf("DecidesAt(%v) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
}

// Property check: a caller that stops early because DecidesAt(template) is true
// must never get a different answer than resolving all three levels together.
func TestDecidesAt_AgreesWithResolvePolicy(t *testing.T) {
	policy := sustainv1alpha1.PolicyAnnotation
	optOut := sustainv1alpha1.OptOutAnnotation
	cases := []struct {
		name                          string
		template, workload, namespace map[string]string
	}{
		{"template decides with policy", ann(policy, "tpl"), ann(policy, "wl"), ann(policy, "ns")},
		{"template decides with opt-out", ann(optOut, "true"), ann(policy, "wl"), ann(policy, "ns")},
		{"template silent, workload decides", nil, ann(policy, "wl"), ann(policy, "ns")},
		{"all silent", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !DecidesAt(tc.template) {
				return // nothing to check here — the whole point is what happens when it's true
			}
			wantName, wantLevel := ResolvePolicy(tc.template, tc.workload, tc.namespace)
			gotName, gotLevel := ResolvePolicy(tc.template, nil, nil)
			if gotName != wantName || gotLevel != wantLevel {
				t.Errorf("DecidesAt(template) was true but resolving template alone = (%q, %q), full resolution = (%q, %q)",
					gotName, gotLevel, wantName, wantLevel)
			}
		})
	}
}
