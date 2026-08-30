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

// ResolvePolicy walks the three annotation levels most-specific-first and
// returns the Policy name the workload opts into, plus the level it came from.
// An empty name (with LevelNone) means the workload is not managed.
//
// At each level, in order pod template → workload metadata → namespace:
//
//   - OptOutAnnotation == "true" ends the walk with no policy. A more specific
//     level's opt-out therefore beats a less specific level's opt-in, which is
//     the whole point of the escape hatch.
//   - a non-empty PolicyAnnotation ends the walk with that name.
//   - anything else falls through to the next level. In particular an EMPTY
//     PolicyAnnotation value falls through rather than opting out: an empty
//     string is what a Helm template renders when a value is unset, and that
//     has always meant "no annotation here", not "exclude me".
//
// Nil maps are fine at any position — a nil map reads as absent, which is
// exactly the semantics a caller wants when it has no such level to offer
// (e.g. a bare Pod has no separate workload metadata).
//
// It lives beside Matches because the two together are the complete answer to
// "does this Policy govern this workload": ResolvePolicy is the workload's
// opt-in, Matches is the Policy's consent. Callers must apply BOTH — a
// Namespace naming a Policy whose selector does not reach it is not a match.
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

// DecidesAt reports whether a single annotation level, taken alone, already
// decides ResolvePolicy's outcome — an opt-out or a non-empty policy name —
// without needing to consult any less specific level.
//
// It exists for callers that hold some levels for free (e.g. a workload
// object already fetched off an informer) but must pay an apiserver read for
// a less specific one (e.g. the Namespace, or an owning workload the caller
// has not fetched yet). Such a caller can check DecidesAt on the levels it
// already has before paying for the read: ResolvePolicy would never consult
// a less specific level once a more specific one decides, so fetching it
// first buys nothing. See internal/controller's collectTargets and
// internal/oomwatch's Watcher.Reconcile for the two callers that do this.
//
// DecidesAt shares its per-level test with ResolvePolicy's loop (via
// decidesAt) so the two can never drift on what "decides" means.
func DecidesAt(annotations map[string]string) bool {
	return decidesAt(annotations)
}

// decidesAt is the per-level test ResolvePolicy's walk applies at each level:
// an opt-out or a non-empty policy name ends the walk there; anything else
// (a nil map, an absent key, or an empty policy value) falls through to the
// next, less specific level.
func decidesAt(annotations map[string]string) bool {
	if annotations[sustainv1alpha1.OptOutAnnotation] == "true" {
		return true
	}
	return annotations[sustainv1alpha1.PolicyAnnotation] != ""
}
