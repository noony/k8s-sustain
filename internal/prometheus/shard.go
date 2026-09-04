package prometheus

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// ShardCandidate is one workload eligible for a batched query.
type ShardCandidate struct {
	Identity WorkloadIdentity
	// Containers is the workload's container count; values <= 0 count as 1.
	Containers int
}

// Shard is a single batched query: one namespace, one owner kind, many owner
// names collapsed into an RE2 alternation. Names holds raw, unescaped names;
// escaping happens only in Selector().
type Shard struct {
	Namespace string
	OwnerKind string
	Names     []string
}

// escapedNameAlternation joins the shard's owner names into an RE2 alternation.
// Names are RFC 1123 subdomains and may contain '.', so each is QuoteMeta'd.
// Every selector built from Shard.Names must go through here.
func (s Shard) escapedNameAlternation() string {
	escaped := make([]string, len(s.Names))
	for i, n := range s.Names {
		escaped[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(escaped, "|")
}

// Selector renders the shard's PromQL label-selector fragment. The %q around
// the alternation is a second escaping layer on top of QuoteMeta: Prometheus's
// lexer un-escapes string literals before the regex engine sees them, and a
// lone `\.` is a parse error.
func (s Shard) Selector() string {
	return fmt.Sprintf(`{namespace=%q,owner_kind=%q,owner_name=~%q}`, s.Namespace, s.OwnerKind, s.escapedNameAlternation())
}

// shardGroupKey identifies one (namespace, owner_kind) group. Shards never
// span groups: label matchers combine as a cross-product, so a wider shard
// would load and discard unrequested series.
type shardGroupKey struct {
	namespace string
	ownerKind string
}

// maxShardMembers caps names per shard regardless of sample cost. Cost scales
// with the window, so a short window would otherwise permit a selector
// hundreds of kilobytes long. 1000 is above what the shipped defaults produce.
const maxShardMembers = 1000

// BuildShards partitions candidates into shards, one per (namespace,
// owner_kind) pair, closing a shard when the next member would exceed
// maxSamples (containers * windowMinutes, summed) or maxShardMembers.
// Candidates with an empty namespace, kind or name are returned in dropped;
// output order is deterministic, and an over-budget workload still gets a
// shard of its own.
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

// unparseableWindowMinutes is WindowMinutes' fallback: 30 days, an overestimate.
const unparseableWindowMinutes = 30 * 24 * 60

// WindowMinutes converts a Prometheus duration string to whole minutes for
// BuildShards. Failures overestimate: sub-minute windows floor at 1 and an
// unparseable window counts as 30 days, since underestimating packs shards
// past --query.max-samples.
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
