// Package policymatchtest provides the shared annotation-resolution contract
// table that the controller, the webhook and the dashboard each replay against
// their own wiring. It is a separate package because a _test.go helper cannot
// be imported by another package's tests, and a fixture does not belong in the
// production policymatch package.
package policymatchtest

import (
	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
)

// AnnotationCase is one row of the cross-component contract table.
type AnnotationCase struct {
	Name                          string
	Template, Workload, Namespace map[string]string
	WantPolicy                    string
	WantLevel                     policymatch.Level
}

// AnnotationCases is the contract table each component replays against its own
// wiring, so no component can quietly resolve the annotation differently. Every
// case names policy "p" when it opts in, so component tests can assert against
// a single fixture Policy.
func AnnotationCases() []AnnotationCase {
	pol := func(v string) map[string]string { return map[string]string{sustainv1alpha1.PolicyAnnotation: v} }
	out := func() map[string]string { return map[string]string{sustainv1alpha1.OptOutAnnotation: "true"} }
	return []AnnotationCase{
		{Name: "pod template", Template: pol("p"), WantPolicy: "p", WantLevel: policymatch.LevelPodTemplate},
		{Name: "workload metadata", Workload: pol("p"), WantPolicy: "p", WantLevel: policymatch.LevelWorkload},
		{Name: "namespace", Namespace: pol("p"), WantPolicy: "p", WantLevel: policymatch.LevelNamespace},
		{Name: "workload beats namespace", Workload: pol("p"), Namespace: pol("other"), WantPolicy: "p", WantLevel: policymatch.LevelWorkload},
		{Name: "template beats both", Template: pol("p"), Workload: pol("other"), Namespace: pol("other"), WantPolicy: "p", WantLevel: policymatch.LevelPodTemplate},
		{Name: "workload opt-out beats namespace opt-in", Workload: out(), Namespace: pol("p"), WantPolicy: "", WantLevel: policymatch.LevelNone},
		{Name: "nothing anywhere", WantPolicy: "", WantLevel: policymatch.LevelNone},
	}
}
