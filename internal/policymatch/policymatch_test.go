package policymatch

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestMatches_EmptySelectorMatchesEverything(t *testing.T) {
	p := &sustainv1alpha1.Policy{}
	if !Matches(p, "default", nil, nil) {
		t.Errorf("empty selector should match")
	}
	if !Matches(p, "production", map[string]string{"app": "x"}, nil) {
		t.Errorf("empty selector should match any labels")
	}
}

func TestMatches_ExcludedNamespaceWins(t *testing.T) {
	p := &sustainv1alpha1.Policy{}
	if Matches(p, "kube-system", nil, []string{"kube-system"}) {
		t.Errorf("namespace in excluded list must not match")
	}
}

func TestMatches_SelectorNamespaces(t *testing.T) {
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				Namespaces: []string{"production", "staging"},
			},
		},
	}
	if !Matches(p, "production", nil, nil) {
		t.Errorf("production should match")
	}
	if Matches(p, "default", nil, nil) {
		t.Errorf("default not in namespaces list should not match")
	}
}

func TestMatches_EmptyNamespacesList_IsClusterWide(t *testing.T) {
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{Namespaces: []string{}},
		},
	}
	if !Matches(p, "anywhere", nil, nil) {
		t.Errorf("empty namespaces list is cluster-wide")
	}
}

func TestMatches_LabelSelector_MatchLabels(t *testing.T) {
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "platform"},
				},
			},
		},
	}
	if !Matches(p, "default", map[string]string{"team": "platform", "app": "x"}, nil) {
		t.Errorf("expected match on label team=platform")
	}
	if Matches(p, "default", map[string]string{"team": "other"}, nil) {
		t.Errorf("expected no match when label differs")
	}
	if Matches(p, "default", nil, nil) {
		t.Errorf("nil labels must not match a non-empty selector")
	}
}

func TestMatches_LabelSelector_MatchExpressions(t *testing.T) {
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "app",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"api", "worker"},
					}},
				},
			},
		},
	}
	if !Matches(p, "default", map[string]string{"app": "api"}, nil) {
		t.Errorf("expected match for app=api")
	}
	if Matches(p, "default", map[string]string{"app": "frontend"}, nil) {
		t.Errorf("expected no match for app=frontend")
	}
}

func TestMatches_EmptyLabelSelector_IsMatchEverything(t *testing.T) {
	// An empty (non-nil) LabelSelector means "match everything" per
	// LabelSelectorAsSelector semantics.
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{},
			},
		},
	}
	if !Matches(p, "default", map[string]string{"any": "label"}, nil) {
		t.Errorf("empty (non-nil) LabelSelector should match all")
	}
	if !Matches(p, "default", nil, nil) {
		t.Errorf("empty LabelSelector should match pods with no labels")
	}
}

func TestMatches_InvalidLabelSelector_DoesNotPanic(t *testing.T) {
	// A malformed selector (e.g. invalid operator) must be treated as
	// non-matching rather than crashing.
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      "app",
						Operator: "BogusOp",
					}},
				},
			},
		},
	}
	if Matches(p, "default", map[string]string{"app": "x"}, nil) {
		t.Errorf("invalid selector must not match")
	}
}

func TestMatches_AllPredicatesCombined(t *testing.T) {
	p := &sustainv1alpha1.Policy{
		Spec: sustainv1alpha1.PolicySpec{
			Selector: sustainv1alpha1.PolicySelector{
				Namespaces: []string{"production"},
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"team": "platform"},
				},
			},
		},
	}
	labels := map[string]string{"team": "platform"}
	if !Matches(p, "production", labels, []string{"kube-system"}) {
		t.Errorf("should match — ns in selector, labels match, not excluded")
	}
	// Excluded wins.
	if Matches(p, "production", labels, []string{"production"}) {
		t.Errorf("excluded namespace must not match even when selector matches")
	}
	// Wrong namespace.
	if Matches(p, "staging", labels, nil) {
		t.Errorf("staging not in selector.namespaces must not match")
	}
	// Wrong labels.
	if Matches(p, "production", map[string]string{"team": "other"}, nil) {
		t.Errorf("non-matching labels must not match")
	}
}
