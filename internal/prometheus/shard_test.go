package prometheus

import (
	"fmt"
	"strings"
	"testing"
)

func cand(ns, kind, name string, containers int) ShardCandidate {
	return ShardCandidate{
		Identity:   WorkloadIdentity{Namespace: ns, OwnerKind: kind, OwnerName: name},
		Containers: containers,
	}
}

func TestBuildShardsSeparatesNamespaceAndKind(t *testing.T) {
	shards, dropped := BuildShards([]ShardCandidate{
		cand("prod", "Deployment", "api", 1),
		cand("prod", "Deployment", "web", 1),
		cand("prod", "StatefulSet", "db", 1),
		cand("staging", "Deployment", "api", 1),
	}, 60, 1_000_000)

	if len(dropped) != 0 {
		t.Fatalf("got %d dropped want 0: %+v", len(dropped), dropped)
	}
	if len(shards) != 3 {
		t.Fatalf("got %d shards want 3 (prod/Deployment, prod/StatefulSet, staging/Deployment): %+v", len(shards), shards)
	}
	seen := map[string]bool{}
	for _, s := range shards {
		key := s.Namespace + "/" + s.OwnerKind
		if seen[key] {
			t.Fatalf("two shards share namespace+kind %q: %+v", key, shards)
		}
		seen[key] = true
	}
}

func TestBuildShardsRespectsSampleBudget(t *testing.T) {
	// 7d window = 10080 minutes. 3 containers each => 30240 samples per workload.
	// Budget 100000 fits 3 workloads (90720) but not 4 (120960).
	var cands []ShardCandidate
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		cands = append(cands, cand("prod", "Deployment", n, 3))
	}
	shards, dropped := BuildShards(cands, 10080, 100_000)

	if len(dropped) != 0 {
		t.Fatalf("got %d dropped want 0: %+v", len(dropped), dropped)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards want 2: %+v", len(shards), shards)
	}
	if len(shards[0].Names) != 3 {
		t.Fatalf("first shard: got %d names want 3: %+v", len(shards[0].Names), shards[0])
	}
	if len(shards[1].Names) != 2 {
		t.Fatalf("second shard: got %d names want 2: %+v", len(shards[1].Names), shards[1])
	}
}

// A single workload whose own projection exceeds the budget still gets a shard.
// Splitting further is impossible, and dropping it would silently stop
// recommending for that workload — a silent regression is worse than one
// oversized query that Prometheus may reject loudly.
func TestBuildShardsEmitsOversizedWorkloadAlone(t *testing.T) {
	shards, dropped := BuildShards([]ShardCandidate{
		cand("prod", "Deployment", "huge", 50),
		cand("prod", "Deployment", "small", 1),
	}, 43200, 100_000)

	if len(dropped) != 0 {
		t.Fatalf("got %d dropped want 0: %+v", len(dropped), dropped)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards want 2: %+v", len(shards), shards)
	}
	// Names sort within a group, so "huge" precedes "small".
	if len(shards[0].Names) != 1 || shards[0].Names[0] != "huge" {
		t.Fatalf("oversized workload should occupy its own shard: %+v", shards[0])
	}
}

// Names containing RE2 metacharacters must be RETAINED, not dropped: they are
// legitimate Kubernetes object names (RFC 1123 subdomains permit `.`), and the
// only safe place to deal with the metacharacter is at Selector() render time,
// via escaping. Shard.Names holds the raw name unchanged.
func TestBuildShardsEscapesRegexMetacharacterNames(t *testing.T) {
	shards, dropped := BuildShards([]ShardCandidate{
		cand("prod", "Deployment", "good", 1),
		cand("prod", "Deployment", "bad|name", 1),
		cand("prod", "Deployment", ".*", 1),
	}, 60, 1_000_000)

	if len(dropped) != 0 {
		t.Fatalf("got %d dropped want 0: %+v", len(dropped), dropped)
	}
	if len(shards) != 1 {
		t.Fatalf("got %d shards want 1: %+v", len(shards), shards)
	}
	names := shards[0].Names
	if len(names) != 3 {
		t.Fatalf("got %d names want 3 (all retained, unescaped): %+v", len(names), names)
	}
	want := map[string]bool{"good": true, "bad|name": true, ".*": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected name %q in Names, or it was escaped: %+v", n, names)
		}
	}

	// Names are escaped twice: QuoteMeta for RE2, then again because PromQL's
	// lexer un-escapes string literals before the regex engine sees them — a
	// lone backslash there is a parse error. See Selector's doc comment.
	sel := shards[0].Selector()
	for _, escaped := range []string{`bad\\|name`, `\\.\\*`, "good"} {
		if !strings.Contains(sel, escaped) {
			t.Fatalf("selector missing double-escaped form %q: %s", escaped, sel)
		}
	}
}

// The real-world case the escaping fix exists for: Deployment, StatefulSet,
// Job, CronJob and Pod names are all RFC 1123 SUBDOMAINS, which (unlike RFC
// 1123 LABELS) explicitly permit `.` as a separator — e.g. "payments.worker".
// `.` is an RE2 metacharacter. A version of BuildShards that dropped such
// names would silently and permanently stop recommending for them.
func TestBuildShardsRoundTripsDottedName(t *testing.T) {
	shards, dropped := BuildShards([]ShardCandidate{
		cand("prod", "Deployment", "payments.worker", 1),
	}, 60, 1_000_000)

	if len(dropped) != 0 {
		t.Fatalf("payments.worker must not be dropped: %+v", dropped)
	}
	if len(shards) != 1 || len(shards[0].Names) != 1 || shards[0].Names[0] != "payments.worker" {
		t.Fatalf("want one shard containing raw name %q: %+v", "payments.worker", shards)
	}
	got := shards[0].Selector()
	// Double backslash, not single: QuoteMeta's "payments\.worker" survives
	// PromQL's string-literal un-escaping only if the query text doubles it.
	want := `{namespace="prod",owner_kind="Deployment",owner_name=~"payments\\.worker"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Hyphens are RE2-literal and by far the most common Kubernetes naming
// convention. It would be easy for an over-eager escaping implementation to
// mangle them; confirm they round-trip byte-for-byte.
func TestBuildShardsRoundTripsHyphenatedName(t *testing.T) {
	shards, dropped := BuildShards([]ShardCandidate{
		cand("prod", "Deployment", "api-server", 1),
	}, 60, 1_000_000)

	if len(dropped) != 0 {
		t.Fatalf("api-server must not be dropped: %+v", dropped)
	}
	got := shards[0].Selector()
	want := `{namespace="prod",owner_kind="Deployment",owner_name=~"api-server"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Only truly unusable candidates — those missing an identity label needed to
// build a matcher at all — are dropped, and BuildShards must report them
// rather than silently swallowing them.
func TestBuildShardsDropsIncompleteIdentitiesAndReportsThem(t *testing.T) {
	empty := cand("", "Deployment", "api", 1)
	noKind := cand("prod", "", "api", 1)
	noName := cand("prod", "Deployment", "", 1)
	good := cand("prod", "Deployment", "ok", 1)

	shards, dropped := BuildShards([]ShardCandidate{empty, noKind, noName, good}, 60, 1_000_000)

	if len(shards) != 1 || len(shards[0].Names) != 1 || shards[0].Names[0] != "ok" {
		t.Fatalf("got shards %+v, want exactly one shard containing \"ok\"", shards)
	}
	if len(dropped) != 3 {
		t.Fatalf("got %d dropped want 3: %+v", len(dropped), dropped)
	}
}

func TestShardSelectorIsExactLiteralAlternation(t *testing.T) {
	s := Shard{Namespace: "prod", OwnerKind: "Deployment", Names: []string{"api", "web"}}
	got := s.Selector()
	want := `{namespace="prod",owner_kind="Deployment",owner_name=~"api|web"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWindowMinutesFloorsAtOne(t *testing.T) {
	if got := WindowMinutes("7d"); got != 10080 {
		t.Fatalf("7d: got %d want 10080", got)
	}
	if got := WindowMinutes("30s"); got != 1 {
		t.Fatalf("30s: got %d want 1 (floored)", got)
	}
}

// An unparseable window must overestimate cost, not underestimate it: a
// smaller-than-truth window packs too many workloads into a shard, which is
// precisely how a query exceeds --query.max-samples and fails outright for
// every workload folded into it. The safe direction is to assume the most
// expensive plausible window (30 days) rather than the cheapest.
func TestWindowMinutesOverestimatesOnUnparseableInput(t *testing.T) {
	got := WindowMinutes("not-a-duration")
	want := 30 * 24 * 60
	if got != want {
		t.Fatalf("garbage: got %d want %d (30d equivalent, the expensive assumption)", got, want)
	}
}

// The sample budget bounds how many SAMPLES a shard query loads, not how many
// NAMES it names. Those diverge as the window shrinks: cost is
// containers*windowMinutes, so at a 1h window the shipped 10M budget permits
// ~166k names in one shard, every one of them rendered into a single RE2
// alternation by Selector(). That is a query hundreds of kilobytes long whose
// sample count is entirely legal, so the budget cannot see it.
func TestBuildShards_CapsMemberCountWhenSampleBudgetIsNotBinding(t *testing.T) {
	const (
		workloads     = 5000
		windowMinutes = 60 // 1h: cost 60/workload, so 10M would allow ~166k
		budget        = 10_000_000
	)
	// Names are realistic length on purpose. Selector size is names x name
	// length, so a fixture of short synthetic names ("app-00001") stays under
	// any sane byte budget even uncapped, and the test would pass against the
	// bug it exists to catch.
	const namePrefix = "checkout-service-worker-deployment-canary-"
	cands := make([]ShardCandidate, 0, workloads)
	for i := range workloads {
		cands = append(cands, ShardCandidate{
			Identity: WorkloadIdentity{
				Namespace: "prod", OwnerKind: "Deployment",
				OwnerName: fmt.Sprintf("%s%05d", namePrefix, i),
			},
			Containers: 1,
		})
	}

	shards, dropped := BuildShards(cands, windowMinutes, budget)
	if len(dropped) != 0 {
		t.Fatalf("dropped %d candidates, want 0", len(dropped))
	}

	// Asserted against an absolute query-size budget, NOT against
	// maxShardMembers: comparing the output to the very constant under test
	// makes the assertion vacuous the moment someone raises that constant,
	// which is exactly the regression this is meant to catch. 64 KiB is far
	// above what the cap permits with these names (1000 x ~48B ≈ 47 KiB) and
	// far below the ~240 KiB a single uncapped shard of 5000 would render.
	const maxSelectorBytes = 100 << 10
	total := 0
	for _, s := range shards {
		if got := len(s.Selector()); got > maxSelectorBytes {
			t.Errorf("shard renders a %d-byte selector for %d names, above the %d-byte budget: a short "+
				"policy window makes the sample budget non-binding, so without a member cap Selector() "+
				"emits an unbounded RE2 alternation", got, len(s.Names), maxSelectorBytes)
		}
		total += len(s.Names)
	}
	if total != workloads {
		t.Errorf("shards cover %d names, want all %d — the cap must split, never drop", total, workloads)
	}
}

// The cap must stay inert at the shipped defaults, or it would silently
// multiply query count for every normal install. A 7d window at 2 containers
// closes shards on sample cost at ~496 names, well under the cap.
func TestBuildShards_MemberCapDoesNotBiteAtShippedDefaults(t *testing.T) {
	const workloads = 5000
	cands := make([]ShardCandidate, 0, workloads)
	for i := range workloads {
		cands = append(cands, ShardCandidate{
			Identity:   WorkloadIdentity{Namespace: "prod", OwnerKind: "Deployment", OwnerName: fmt.Sprintf("app-%05d", i)},
			Containers: 2,
		})
	}
	shards, _ := BuildShards(cands, WindowMinutes("168h"), 10_000_000)
	for _, s := range shards {
		if len(s.Names) >= maxShardMembers {
			t.Fatalf("a default-config shard hit the member cap (%d names): the cap is meant to engage only "+
				"where the sample budget has stopped being the binding constraint", len(s.Names))
		}
	}
}
