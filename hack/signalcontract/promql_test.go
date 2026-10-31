package signalcontract

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The repository does not depend on the PromQL parser, so expressions are
// read textually. Both readers fail closed: an expression they cannot read
// is reported, never waved through.

// skeleton blanks expr's string literals and drops its comments, so neither
// can pass for a metric name, a label or a parenthesis.
func skeleton(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; c {
		case '"', '\'', '`':
			j := i + 1
			for j < len(expr) && expr[j] != c {
				if expr[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			b.WriteString(`""`)
			i = j
		case '#':
			for i+1 < len(expr) && expr[i+1] != '\n' {
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

var (
	matchersAndRanges = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]`)
	labelLists        = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	token             = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*|[0-9][A-Za-z0-9_.]*`)
	keywords          = []string{
		"and", "or", "unless", "by", "without", "on", "ignoring", "group_left", "group_right",
		"bool", "offset", "inf", "nan",
	}
)

// metricNames lists, sorted, the metric names expr selects: every identifier
// that is not a function or aggregation call, a keyword, a label or a
// number.
func metricNames(expr string) []string {
	sk := labelLists.ReplaceAllString(matchersAndRanges.ReplaceAllString(skeleton(expr), " "), " ")
	var out []string
	for _, loc := range token.FindAllStringIndex(sk, -1) {
		name := sk[loc[0]:loc[1]]
		if name[0] >= '0' && name[0] <= '9' || slices.Contains(keywords, strings.ToLower(name)) {
			continue
		}
		if rest := strings.TrimLeft(sk[loc[1]:], " \t\r\n"); strings.HasPrefix(rest, "(") {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

var aggregation = regexp.MustCompile(
	`^\s*(?:sum|min|max|avg|group|count|stddev|stdvar|topk|bottomk|quantile|count_values|limitk|limit_ratio)\s+by\s*\(([^)]*)\)\s*\(`)

// aggregatedBy returns, sorted, the labels expr aggregates by when its whole
// expression is one `<aggregation> by (<labels>) (...)`.
func aggregatedBy(expr string) ([]string, bool) {
	sk := skeleton(expr)
	m := aggregation.FindStringSubmatchIndex(sk)
	if m == nil {
		return nil, false
	}
	depth := 0
	for i := m[1] - 1; i < len(sk); i++ {
		switch sk[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth > 0 {
				continue
			}
			if strings.TrimSpace(sk[i+1:]) != "" {
				return nil, false
			}
			var labels []string
			for l := range strings.SplitSeq(sk[m[2]:m[3]], ",") {
				if l = strings.TrimSpace(l); l != "" {
					labels = append(labels, l)
				}
			}
			slices.Sort(labels)
			return labels, true
		}
	}
	return nil, false
}

func TestMetricNames(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []string
	}{
		{
			"functions, matchers, ranges and label lists",
			`max by (namespace, pod) (rate(cpu_total{container!="", pod=~"a|b"}[1m:10s]))
			 * on(namespace, pod) group_left(owner_kind) k8s_sustain:pod_workload`,
			[]string{"cpu_total", "k8s_sustain:pod_workload"},
		},
		{
			"strings and comments",
			`label_replace(kube_pod_owner, "job_name", "$1", "owner_name", "(.*)") # not_a_metric here`,
			[]string{"kube_pod_owner"},
		},
		{
			"group_left without labels, bool, numbers and offsets",
			`a unless on(ns) b * on(ns, job) group_left
			 max by (ns, job) (c) > bool 0 or d offset 5m == 1e3`,
			[]string{"a", "b", "c", "d"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := metricNames(tc.expr); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAggregatedBy(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		want   []string
		wantOK bool
	}{
		{"one aggregation", "max by (namespace, container) (\n  a * on(pod) group_left(x) b\n)\n", []string{"container", "namespace"}, true},
		{"a string holding a parenthesis", `sum by (a) (label_replace(x, "y", "(", "", ""))`, []string{"a"}, true},
		{"anything after the aggregation", "max by (a) (x) or y", nil, false},
		{"labels after the body", "max (x) by (a)", nil, false},
		{"without", "max without (a) (x)", nil, false},
		{"no aggregation", "x", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := aggregatedBy(tc.expr)
			if ok != tc.wantOK || !slices.Equal(got, tc.want) {
				t.Errorf("got %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
