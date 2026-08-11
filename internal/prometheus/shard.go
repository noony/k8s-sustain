package prometheus

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// ShardCandidate is one workload eligible for inclusion in a batched query,
// carrying just enough information to group it and to project its sample cost.
type ShardCandidate struct {
	Identity WorkloadIdentity
	// Containers is the number of containers this workload contributes to a
	// range-vector query (each container is its own time series). Values <= 0
	// are treated as 1 by BuildShards.
	Containers int
}

// Shard is a single batched query: one namespace, one owner kind, many owner
// names collapsed into an RE2 alternation.
//
// Names holds the RAW, un-escaped owner names — never the RE2-escaped form
// used in the rendered selector. A later per-workload fallback path
// reconstructs WorkloadIdentity{Namespace, OwnerKind, OwnerName} straight from
// Names and issues exact-match (owner_name="...") queries; those need the
// literal name, e.g. "payments.worker" — an escaped form would match nothing.
// Escaping is purely a rendering concern of Selector(), applied at the last
// possible moment, never stored.
type Shard struct {
	Namespace string
	OwnerKind string
	Names     []string
}

// escapedNameAlternation returns the RE2 alternation of this shard's owner
// names, each escaped with regexp.QuoteMeta before being joined with `|`.
//
// This is the ONLY place in the package that escapes and joins Shard.Names.
// Shard.Names holds raw Kubernetes object names, which are RFC 1123
// SUBDOMAINS (not labels) and so may legitimately contain `.` — an RE2
// metacharacter — as in a Deployment named "payments.worker". QuoteMeta is
// total (every string escapes safely without changing what it matches
// literally), so escaping here, rather than filtering names upstream, is both
// correct and lossless: the escaped form matches exactly the literal name and
// nothing more.
//
// Every selector this package builds against Shard.Names — the plain
// identity selector (Selector) and the combined OOM __name__+identity
// selector (oomShardSelector in batch.go) — MUST call this method rather than
// re-deriving the alternation themselves. A second, independent
// escape-and-join implementation is exactly how the two selectors would drift
// apart: this was a real bug caught in review, where the OOM selector's
// obvious standalone implementation joined shard.Names raw, silently losing
// the escaping Selector already had.
func (s Shard) escapedNameAlternation() string {
	escaped := make([]string, len(s.Names))
	for i, n := range s.Names {
		escaped[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(escaped, "|")
}

// Selector renders the shard's `{namespace=...,owner_kind=...,owner_name=~...}`
// PromQL label-selector fragment.
//
// The name alternation (see escapedNameAlternation) is wrapped with %q — a
// SECOND, independent layer of escaping on top of QuoteMeta's, not a
// substitute for it. This string is PromQL source text, and Prometheus's own
// lexer un-escapes double-quoted string literals before ever handing a value
// to the regex engine (promql/parser/lex.go's lexEscape). Verified directly
// against that lexer: a lone backslash followed by an unrecognised escape
// character (`\.` is one — only a small fixed set like \n, \t, \\, \" are
// recognised) is a PARSE ERROR, not a passthrough — so a QuoteMeta'd `\.`
// cannot appear in the query text as a single backslash; it must appear as
// `\\.` for Prometheus's parser to hand the regex engine back the single
// `\.` QuoteMeta intended. %q performs exactly that doubling for every
// backslash byte, which is why it wraps the whole already-escaped alternation
// rather than each raw name before escaping — namespace and owner_kind carry
// no regex meaning, so their %q is just the ordinary exact-match quoting used
// throughout this package (see workloadSelector).
func (s Shard) Selector() string {
	return fmt.Sprintf(`{namespace=%q,owner_kind=%q,owner_name=~%q}`, s.Namespace, s.OwnerKind, s.escapedNameAlternation())
}

// shardGroupKey identifies one (namespace, owner_kind) group. Grouping is
// restricted to this granularity — never wider — because label matchers
// combine as a cross-product: a single query spanning `{namespace=~"prod|staging",
// owner_name=~"api|web"}` would also match staging/api and prod/web even if
// only prod/api and staging/web were requested. Indexing results by full
// identity (see IdentityValues) keeps that *correct* — unrequested rows are
// just ignored — but Prometheus still loads and discards those extra samples,
// which is pure waste at the scale a policy spanning dozens of namespaces
// would produce. Pinning namespace and owner_kind per shard makes every
// remaining matcher (owner_name) an exact, non-cross-product alternation.
type shardGroupKey struct {
	namespace string
	ownerKind string
}

// maxShardMembers caps how many workload names one shard may hold, regardless
// of their sample cost.
//
// The sample budget alone does not bound this, because cost scales with the
// window: at the shipped 10M budget a 7d single-container shard already permits
// ~992 names, a 24h window ~6,900, and a 1h window ~166,000. Every name is
// rendered into one RE2 alternation by Selector(), so without a cap a short
// policy window silently produces a single query hundreds of kilobytes long —
// expensive to transfer, parse and compile — while the sample count, the only
// thing the budget measures, stays perfectly legal. (The local scenario harness
// defaults to WINDOW=10m, so this is reachable in testing, not just in theory.)
//
// 1000 is deliberately above what the shipped defaults produce — a 7d window at
// 2 containers caps at ~496 names on sample cost first — so this changes
// nothing for a default install and engages only where the sample budget has
// stopped being the binding constraint.
const maxShardMembers = 1000

// BuildShards partitions candidates into batched-query shards, one per
// (namespace, owner_kind) pair, respecting a per-shard sample budget. It
// returns the shards plus any candidates it could not place, so callers can
// log or meter the exclusion instead of it becoming a silent blind spot.
//
// windowMinutes and maxSamples together bound the raw samples Prometheus must
// load into memory to evaluate a range-vector query like
// `quantile_over_time(q, rule[window])` before it can compute anything —
// Prometheus enforces this with --query.max-samples (default 50,000,000) and
// rejects an over-budget query ENTIRELY, failing every workload folded into
// it. The right shard size is therefore a function of the actual window, not
// a fixed workload count: a 200-workload shard is comfortable at a 7d window
// (~6M samples) but can blow the default cap at 30d (~26M for the same 200,
// and 400 would exceed it outright). Callers get windowMinutes from
// WindowMinutes and maxSamples from their own configured budget (typically a
// safety margin under --query.max-samples).
//
// A candidate is dropped into the returned slice — never silently folded
// elsewhere, never dropped without a trace — only when Namespace, OwnerKind,
// or OwnerName is empty: none of those can be turned into a matcher, and none
// of them can legitimately come from the apiserver. OwnerName is NOT
// validated or filtered otherwise: any other name, including one containing
// RE2 metacharacters such as `.`, is retained verbatim in Shard.Names and
// escaped only at Selector() render time (see Selector's doc comment) — an
// earlier version of this function rejected such names outright on the false
// premise that Kubernetes names never contain regex metacharacters, which
// silently dropped every workload with a dotted name from recommendations.
//
// Within a group, members are sorted by name before accumulation, and groups
// themselves are visited in sorted (namespace, owner_kind) order — not
// insertion order — so shard output is fully deterministic regardless of the
// order candidates arrive in (a Go map range, for instance, would not
// preserve one): identical input always yields identical shards in identical
// order, with identical name order inside each shard.
//
// A shard accumulates members until the NEXT member would push its running
// sample cost (containers * windowMinutes, summed) over maxSamples OR the
// member count to maxShardMembers, at which point the shard is closed and a new
// one started. A shard is never emitted empty: if a single workload's own cost
// already exceeds maxSamples, it still gets a shard of its own rather than
// being dropped or endlessly deferred — splitting a single workload's query
// further isn't possible, and silently dropping it would silently stop
// recommending for it, which is worse than sending one oversized query that
// Prometheus may reject loudly (and visibly, in logs/metrics) instead.
//
// The two limits guard different resources, which is why the member cap is not
// redundant with the sample budget — see maxShardMembers.
func BuildShards(cands []ShardCandidate, windowMinutes, maxSamples int) (shards []Shard, dropped []ShardCandidate) {
	if windowMinutes <= 0 {
		windowMinutes = 1
	}

	type member struct {
		name string
		cost int
	}

	groupOrder := make([]shardGroupKey, 0)
	groups := make(map[shardGroupKey][]member)

	for _, c := range cands {
		ns := c.Identity.Namespace
		kind := c.Identity.OwnerKind
		name := c.Identity.OwnerName
		if ns == "" || kind == "" || name == "" {
			dropped = append(dropped, c)
			continue
		}
		containers := c.Containers
		if containers <= 0 {
			containers = 1
		}
		key := shardGroupKey{namespace: ns, ownerKind: kind}
		if _, ok := groups[key]; !ok {
			groupOrder = append(groupOrder, key)
		}
		groups[key] = append(groups[key], member{name: name, cost: containers * windowMinutes})
	}

	sort.Slice(groupOrder, func(i, j int) bool {
		if groupOrder[i].namespace != groupOrder[j].namespace {
			return groupOrder[i].namespace < groupOrder[j].namespace
		}
		return groupOrder[i].ownerKind < groupOrder[j].ownerKind
	})

	for _, key := range groupOrder {
		members := groups[key]
		sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })

		var names []string
		total := 0
		flush := func() {
			if len(names) == 0 {
				return
			}
			shards = append(shards, Shard{Namespace: key.namespace, OwnerKind: key.ownerKind, Names: names})
			names = nil
			total = 0
		}
		for _, m := range members {
			if len(names) > 0 && (total+m.cost > maxSamples || len(names) >= maxShardMembers) {
				flush()
			}
			names = append(names, m.name)
			total += m.cost
		}
		flush()
	}

	return shards, dropped
}

// unparseableWindowMinutes is the fallback WindowMinutes returns when it
// cannot parse the input, expressed as whole minutes in a 30-day window. See
// WindowMinutes for why this must overestimate rather than underestimate.
const unparseableWindowMinutes = 30 * 24 * 60

// WindowMinutes converts a Prometheus duration string (e.g. "7d", "30s") to
// whole minutes, for use as BuildShards' windowMinutes.
//
// Every failure mode is made to OVERESTIMATE the per-sample cost, never
// underestimate it, because the two directions are not symmetric in
// consequence: overestimating produces smaller, more numerous shards — wasted
// round trips, but each query still succeeds. Underestimating packs too many
// workloads into a shard, which is exactly how a query exceeds
// --query.max-samples and is rejected outright, failing every workload folded
// into it.
//
//   - A valid duration floors at 1 minute (e.g. "30s" -> 1). Flooring a
//     sub-minute window up to a full minute of "cost" overestimates, which is
//     the safe direction here.
//   - An UNPARSEABLE window returns the equivalent of 30 days, not 1 minute.
//     An unparseable window means the caller does not actually know the
//     window length — treating that as "effectively zero cost" (returning 1)
//     would be the unsafe guess, silently permitting the exact oversized-shard
//     failure this whole budgeting scheme exists to prevent. The only safe
//     guess when the cost is unknown is an expensive one.
func WindowMinutes(window string) int {
	d, err := model.ParseDuration(window)
	if err != nil {
		return unparseableWindowMinutes
	}
	minutes := int(time.Duration(d).Minutes())
	if minutes < 1 {
		return 1
	}
	return minutes
}
