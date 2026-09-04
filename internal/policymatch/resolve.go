package policymatch

import (
	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// Level names the annotation level an opt-in was resolved from. The string
// values are stable and safe to log or expose.
type Level string

const (
	// LevelNone means the workload opted into nothing.
	LevelNone Level = ""
	// LevelPodTemplate is spec.template.metadata.annotations (or, for a bare
	// Pod, the Pod's own annotations).
	LevelPodTemplate Level = "podTemplate"
	// LevelWorkload is the workload object's own metadata.annotations.
	LevelWorkload Level = "workload"
	// LevelNamespace is the Namespace object's metadata.annotations.
	LevelNamespace Level = "namespace"
)

// ResolvePolicy walks pod template → workload metadata → namespace and returns
// the Policy name the workload opts into plus the level it came from; an empty
// name with LevelNone means unmanaged. At each level an opt-out ends the walk
// with no policy (so a more specific opt-out beats a less specific opt-in) and
// a non-empty PolicyAnnotation ends it with that name. An EMPTY annotation
// value falls through rather than opting out — that is what a Helm template
// renders for an unset value. Nil maps read as absent.
//
// ResolvePolicy is the workload's opt-in; Matches is the Policy's consent.
// Callers must apply BOTH.
func ResolvePolicy(template, workloadMeta, namespace map[string]string) (string, Level) {
	levels := []struct {
		annotations map[string]string
		level       Level
	}{
		{template, LevelPodTemplate},
		{workloadMeta, LevelWorkload},
		{namespace, LevelNamespace},
	}
	for _, l := range levels {
		if decidesAt(l.annotations) {
			if l.annotations[sustainv1alpha1.OptOutAnnotation] == "true" {
				return "", LevelNone
			}
			return l.annotations[sustainv1alpha1.PolicyAnnotation], l.level
		}
	}
	return "", LevelNone
}

// DecidesAt reports whether one annotation level alone already decides
// ResolvePolicy's outcome. It lets a caller holding the specific levels for
// free skip an apiserver read for a less specific one that ResolvePolicy would
// never consult anyway. It shares its test with ResolvePolicy's loop so the two
// cannot drift.
func DecidesAt(annotations map[string]string) bool {
	return decidesAt(annotations)
}

func decidesAt(annotations map[string]string) bool {
	if annotations[sustainv1alpha1.OptOutAnnotation] == "true" {
		return true
	}
	return annotations[sustainv1alpha1.PolicyAnnotation] != ""
}
