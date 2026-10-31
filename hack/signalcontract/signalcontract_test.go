package signalcontract

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"sigs.k8s.io/yaml"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
)

const (
	repoRoot    = "../.."
	valuesFile  = "charts/k8s-sustain/values.yaml"
	fixturesDir = "charts/k8s-sustain/tests/promtool"
)

type relabel struct {
	Action       string   `json:"action"`
	SourceLabels []string `json:"source_labels"`
	Separator    string   `json:"separator"`
	Regex        string   `json:"regex"`
	TargetLabel  string   `json:"target_label"`
	Replacement  *string  `json:"replacement"`
}

type scrapeJob struct {
	JobName              string    `json:"job_name"`
	Enabled              *bool     `json:"enabled"`
	MetricRelabelConfigs []relabel `json:"metric_relabel_configs"`
}

type values struct {
	PrometheusRule struct {
		Groups []struct {
			Name     string `json:"name"`
			Interval string `json:"interval"`
			Rules    []struct {
				Record string `json:"record"`
				Expr   string `json:"expr"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"prometheusRule"`
	Prometheus struct {
		ScrapeConfigs      map[string]scrapeJob `json:"scrapeConfigs"`
		ExtraScrapeConfigs string               `json:"extraScrapeConfigs"`
		KubeStateMetrics   struct {
			MetricAnnotationsAllowList []string `json:"metricAnnotationsAllowList"`
		} `json:"kube-state-metrics"`
	} `json:"prometheus"`
}

type fixture struct {
	Tests []struct {
		PromqlExprTest []struct {
			Expr       string            `json:"expr"`
			ExpSamples []json.RawMessage `json:"exp_samples"`
		} `json:"promql_expr_test"`
	} `json:"tests"`
}

// rule is one definition of a recording rule: several may record one name.
type rule struct {
	group    string
	interval string
	expr     string
}

// scrape is the bundled Prometheus job reading one exporter's metrics. found
// is false when the chart has no such job or disables it.
type scrape struct {
	exporter string
	job      string
	found    bool
	relabels []relabel
}

// keeps reports whether the job ingests metric: it passes every keep and no
// drop on __name__. Prometheus anchors relabel regexes at both ends.
func (s scrape) keeps(metric string) bool {
	for _, r := range s.relabels {
		if !slices.Equal(r.SourceLabels, []string{model.MetricNameLabel}) {
			continue
		}
		matches := regexp.MustCompile(`^(?:` + r.Regex + `)$`).MatchString(metric)
		switch strings.ToLower(r.Action) {
		case "keep":
			if !matches {
				return false
			}
		case "drop":
			if matches {
				return false
			}
		}
	}
	return true
}

// relabel applies the job's metric_relabel_configs to series as Prometheus
// does, and reports whether it keeps the series. An action it cannot apply is
// an error rather than a guess.
func (s scrape) relabel(series map[string]string) (map[string]string, bool, error) {
	out := maps.Clone(series)
	for _, r := range s.relabels {
		re, err := regexp.Compile(`^(?:` + cmp.Or(r.Regex, "(.*)") + `)$`)
		if err != nil {
			return nil, false, err
		}
		values := make([]string, len(r.SourceLabels))
		for i, l := range r.SourceLabels {
			values[i] = out[l]
		}
		value := strings.Join(values, cmp.Or(r.Separator, ";"))
		switch action := strings.ToLower(cmp.Or(r.Action, "replace")); action {
		case "keep", "drop":
			if re.MatchString(value) != (action == "keep") {
				return nil, false, nil
			}
		case "replace":
			match := re.FindStringSubmatchIndex(value)
			if match == nil {
				continue
			}
			replacement := "$1"
			if r.Replacement != nil {
				replacement = *r.Replacement
			}
			if v := string(re.ExpandString(nil, replacement, value, match)); v != "" {
				out[r.TargetLabel] = v
			} else {
				delete(out, r.TargetLabel)
			}
		case "labeldrop", "labelkeep":
			for l := range out {
				if re.MatchString(l) == (action == "labeldrop") {
					delete(out, l)
				}
			}
		default:
			return nil, false, fmt.Errorf("cannot apply relabel action %q", r.Action)
		}
	}
	return out, true, nil
}

// instancesApart reports whether the job keeps two instances of one container
// apart in metric. A restarted container is a new cgroup: cAdvisor names each
// instance in id, its cgroup path, and name, its runtime container ID.
func (s scrape) instancesApart(metric string) (bool, error) {
	var kept []map[string]string
	for _, container := range []string{"0a1b2c", "3d4e5f"} {
		out, ok, err := s.relabel(map[string]string{
			model.MetricNameLabel: metric,
			"namespace":           "shop",
			"pod":                 "api-0",
			"container":           "app",
			"image":               "app:v1",
			"id":                  "/kubepods.slice/kubepods-pod1.slice/cri-containerd-" + container + ".scope",
			"name":                container,
		})
		if err != nil || !ok {
			return false, err
		}
		kept = append(kept, out)
	}
	return !maps.Equal(kept[0], kept[1]), nil
}

// chart is what the contracts are checked against: the recording rules by
// name, the bundled scrape job of each exporter, the pod annotations the
// bundled kube-state-metrics exports, and the rules a promtool fixture
// evaluates with expected samples.
type chart struct {
	rules            map[string][]rule
	cAdvisor         scrape
	kubeStateMetrics scrape
	podAnnotations   []string
	fixtured         map[string]bool
}

func loadChart(t *testing.T) chart {
	t.Helper()
	var v values
	readYAML(t, filepath.Join(repoRoot, valuesFile), &v)

	c := chart{rules: map[string][]rule{}, fixtured: map[string]bool{}}
	for _, g := range v.PrometheusRule.Groups {
		for _, r := range g.Rules {
			if r.Record != "" {
				c.rules[r.Record] = append(c.rules[r.Record], rule{group: g.Name, interval: g.Interval, expr: r.Expr})
			}
		}
	}

	c.cAdvisor = scrape{exporter: "cAdvisor", job: "prometheus.scrapeConfigs.kubernetes-nodes-cadvisor"}
	if job, ok := v.Prometheus.ScrapeConfigs["kubernetes-nodes-cadvisor"]; ok && (job.Enabled == nil || *job.Enabled) {
		c.cAdvisor.found, c.cAdvisor.relabels = true, job.MetricRelabelConfigs
	}
	var extra []scrapeJob
	if err := yaml.Unmarshal([]byte(v.Prometheus.ExtraScrapeConfigs), &extra); err != nil {
		t.Fatalf("prometheus.extraScrapeConfigs: %v", err)
	}
	for _, entry := range v.Prometheus.KubeStateMetrics.MetricAnnotationsAllowList {
		if keys, ok := strings.CutPrefix(entry, "pods=["); ok {
			c.podAnnotations = append(c.podAnnotations, strings.Split(strings.TrimSuffix(keys, "]"), ",")...)
		}
	}
	c.kubeStateMetrics = scrape{exporter: "kube-state-metrics", job: "the kube-state-metrics job in prometheus.extraScrapeConfigs"}
	for _, job := range extra {
		if job.JobName == "kube-state-metrics" {
			c.kubeStateMetrics.found, c.kubeStateMetrics.relabels = true, job.MetricRelabelConfigs
		}
	}

	files, err := filepath.Glob(filepath.Join(repoRoot, fixturesDir, "*_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		var fx fixture
		readYAML(t, f, &fx)
		for _, tc := range fx.Tests {
			for _, e := range tc.PromqlExprTest {
				if len(e.ExpSamples) == 0 {
					continue
				}
				for _, name := range metricNames(e.Expr) {
					c.fixtured[name] = true
				}
			}
		}
	}
	return c
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// through lists what rules compute from, following every rule they read down
// to the series no rule records: the recorded rules they read in turn, and
// those raw metrics.
func (c chart) through(rules []string) (recorded, raw map[string]bool) {
	recorded, raw = map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, d := range c.rules[name] {
			for _, m := range metricNames(d.expr) {
				if _, ok := c.rules[m]; ok {
					recorded[m] = true
					walk(m)
				} else {
					raw[m] = true
				}
			}
		}
	}
	for _, r := range rules {
		walk(r)
	}
	return recorded, raw
}

// check lists every way the chart fails the contracts.
func check(c chart, contracts []recommender.PromContract) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, k := range contracts {
		labels := slices.Sorted(slices.Values(k.Labels))
		allRecorded := true
		for _, name := range k.Rules {
			defs := c.rules[name]
			if len(defs) == 0 {
				report("signal %s reads %s, which prometheusRule.groups in %s does not record", k.Signal, name, valuesFile)
				allRecorded = false
				continue
			}
			for _, d := range defs {
				if k.Interval != 0 {
					every, err := model.ParseDuration(d.interval)
					if err != nil || time.Duration(every) != k.Interval {
						report("signal %s reads %s every %s, but group %s evaluates it at interval %q",
							k.Signal, name, model.Duration(k.Interval), d.group, d.interval)
					}
				}
				by, ok := aggregatedBy(d.expr)
				if !ok {
					report("signal %s reads %s, whose whole expression must be one aggregation `by (%s)`",
						k.Signal, name, strings.Join(labels, ", "))
				} else if !slices.Equal(by, labels) {
					report("signal %s reads %s, which aggregates by (%s), not exactly (%s)",
						k.Signal, name, strings.Join(by, ", "), strings.Join(labels, ", "))
				}
			}
			if !c.fixtured[name] {
				report("signal %s reads %s, which no promtool fixture in %s evaluates with expected samples",
					k.Signal, name, fixturesDir)
			}
		}

		declared := map[string]bool{}
		for _, m := range slices.Concat(k.CAdvisor, k.KubeStateMetrics) {
			declared[m] = true
		}
		if allRecorded {
			recorded, raw := c.through(k.Rules)
			for _, name := range slices.Sorted(maps.Keys(recorded)) {
				if !slices.Contains(k.Rules, name) && !c.fixtured[name] {
					report("signal %s's rules compute from %s, which no promtool fixture in %s evaluates with expected samples",
						k.Signal, name, fixturesDir)
				}
			}
			for _, m := range slices.Sorted(maps.Keys(raw)) {
				if !declared[m] {
					report("signal %s's rules compute from %s, which its PromContract does not declare", k.Signal, m)
				}
			}
			for _, m := range slices.Sorted(maps.Keys(declared)) {
				if !raw[m] {
					report("signal %s declares %s, which none of its rules computes from", k.Signal, m)
				}
			}
		}
		for _, from := range []struct {
			metrics []string
			scrape  scrape
		}{{k.CAdvisor, c.cAdvisor}, {k.KubeStateMetrics, c.kubeStateMetrics}} {
			for _, m := range from.metrics {
				switch s := from.scrape; {
				case !s.found:
					report("signal %s needs %s metric %s, but %s has no %s", k.Signal, s.exporter, m, valuesFile, s.job)
				case !s.keeps(m):
					report("signal %s needs %s metric %s, which the metric_relabel_configs of %s in %s drop",
						k.Signal, s.exporter, m, s.job, valuesFile)
				}
			}
		}
		for _, m := range k.CAdvisorPerInstance {
			s := c.cAdvisor
			if !s.found || !s.keeps(m) {
				continue
			}
			switch apart, err := s.instancesApart(m); {
			case err != nil:
				report("signal %s reads cAdvisor metric %s per container instance, but the metric_relabel_configs of %s in %s cannot be read: %v",
					k.Signal, m, s.job, valuesFile, err)
			case !apart:
				report("signal %s reads cAdvisor metric %s per container instance, but the metric_relabel_configs of %s in %s merge a restarted container's instances",
					k.Signal, m, s.job, valuesFile)
			}
		}
	}
	return problems
}

// The chart provides every signal's contract: its rules, aggregated by its
// labels at its interval, computed from the raw metrics it declares, all of
// them kept by their exporter's keep-list, those read per container instance
// kept apart per instance, and every rule they compute through, theirs
// included, covered by a promtool fixture.
func TestChartProvidesEverySignalsContract(t *testing.T) {
	contracts := recommender.PromContracts()
	if len(contracts) == 0 {
		t.Fatal("no signal declares a PromContract")
	}
	for _, p := range check(loadChart(t), contracts) {
		t.Error(p)
	}
}

const throttleRule = "k8s_sustain:workload_cpu_throttled:ratio"

// Each way a chart can fail a contract is reported on its own, naming what to
// fix, starting from the CPU usage signal's contract the chart provides.
func TestCheckReportsEachBreach(t *testing.T) {
	var cpu recommender.PromContract
	for _, k := range recommender.PromContracts() {
		if k.Signal == "cpu" {
			cpu = k
		}
	}
	if len(cpu.Rules) != 1 {
		t.Fatalf("cpu contract = %+v, want one rule", cpu)
	}
	usageRule := cpu.Rules[0]
	editRule := func(c *chart, edit func(*rule)) {
		defs := slices.Clone(c.rules[usageRule])
		edit(&defs[0])
		c.rules[usageRule] = defs
	}

	cases := []struct {
		name string
		edit func(*chart, *recommender.PromContract)
		want string
	}{
		{
			"a rule the chart does not record",
			func(_ *chart, k *recommender.PromContract) { k.Rules = []string{throttleRule} },
			throttleRule + ", which prometheusRule.groups in " + valuesFile + " does not record",
		},
		{
			"a raw metric the keep-list drops",
			func(c *chart, k *recommender.PromContract) {
				c.rules[throttleRule] = []rule{{group: "throttle", interval: "1m", expr: `
					max by (namespace, owner_kind, owner_name, container) (
					  rate(container_cpu_cfs_throttled_periods_total{container!=""}[5m])
					  * on(namespace, pod) group_left(owner_kind, owner_name) k8s_sustain:pod_workload
					)`}}
				c.fixtured[throttleRule] = true
				k.Rules = []string{throttleRule}
				k.CAdvisor = []string{"container_cpu_cfs_throttled_periods_total"}
			},
			"needs cAdvisor metric container_cpu_cfs_throttled_periods_total, which the metric_relabel_configs of prometheus.scrapeConfigs.kubernetes-nodes-cadvisor",
		},
		{
			"an exporter the chart does not scrape",
			func(c *chart, _ *recommender.PromContract) { c.cAdvisor.found = false },
			"needs cAdvisor metric container_cpu_usage_seconds_total, but " + valuesFile + " has no prometheus.scrapeConfigs.kubernetes-nodes-cadvisor",
		},
		{
			"an exporter's metric declared under another",
			func(_ *chart, k *recommender.PromContract) {
				k.KubeStateMetrics = slices.Concat(k.KubeStateMetrics, k.CAdvisor)
				k.CAdvisor = nil
			},
			"needs kube-state-metrics metric container_cpu_usage_seconds_total, which the metric_relabel_configs of the kube-state-metrics job",
		},
		{
			"a rule aggregating by one more label",
			func(c *chart, _ *recommender.PromContract) {
				editRule(c, func(r *rule) {
					r.expr = "max by (namespace, owner_kind, owner_name, container, pod) (k8s_sustain:container_cpu_usage_by_workload:rate1m)"
				})
			},
			"aggregates by (container, namespace, owner_kind, owner_name, pod), not exactly (container, namespace, owner_kind, owner_name)",
		},
		{
			"a rule that is not one aggregation",
			func(c *chart, _ *recommender.PromContract) {
				editRule(c, func(r *rule) { r.expr = "k8s_sustain:container_cpu_usage_by_workload:rate1m" })
			},
			"whose whole expression must be one aggregation `by (container, namespace, owner_kind, owner_name)`",
		},
		{
			"a rule evaluated at another interval",
			func(c *chart, _ *recommender.PromContract) {
				editRule(c, func(r *rule) { r.interval = "5m" })
			},
			`every 1m, but group k8s_sustain.workload_max_pod evaluates it at interval "5m"`,
		},
		{
			"a rule no promtool fixture evaluates",
			func(c *chart, _ *recommender.PromContract) { delete(c.fixtured, usageRule) },
			"which no promtool fixture in " + fixturesDir + " evaluates with expected samples",
		},
		{
			"a rule computed through that no promtool fixture evaluates",
			func(c *chart, _ *recommender.PromContract) {
				delete(c.fixtured, promclient.MetricContainerCPUUsageByWorkloadRate1m)
			},
			"rules compute from " + promclient.MetricContainerCPUUsageByWorkloadRate1m + ", which no promtool fixture",
		},
		{
			"a raw metric the contract does not declare",
			func(_ *chart, k *recommender.PromContract) { k.CAdvisor = nil },
			"rules compute from container_cpu_usage_seconds_total, which its PromContract does not declare",
		},
		{
			"a declared raw metric no rule computes from",
			func(_ *chart, k *recommender.PromContract) {
				k.CAdvisor = append(slices.Clone(k.CAdvisor), "container_memory_working_set_bytes")
			},
			"declares container_memory_working_set_bytes, which none of its rules computes from",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadChart(t)
			k := cpu
			tc.edit(&c, &k)

			problems := check(c, []recommender.PromContract{k})

			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Errorf("problems = %q, want one containing %q", problems, tc.want)
			}
		})
	}
}

// The OOM floor's killed-at limit reads the memory limit per container
// instance: a cAdvisor scrape merging a restarted container's instances fails
// the OOM floor's contract, and one keeping cAdvisor's own id provides it.
func TestCheckReportsMergedInstances(t *testing.T) {
	var oom recommender.PromContract
	for _, k := range recommender.PromContracts() {
		if k.Signal == "oom" {
			oom = k
		}
	}
	if len(oom.CAdvisorPerInstance) == 0 {
		t.Fatalf("oom contract = %+v, want a cAdvisor metric read per container instance", oom)
	}
	withoutReplace := func(c *chart) {
		c.cAdvisor.relabels = slices.DeleteFunc(slices.Clone(c.cAdvisor.relabels), func(r relabel) bool {
			return r.Action == "replace"
		})
	}

	cases := []struct {
		name string
		edit func(*chart)
		want string
	}{
		{
			"a scrape dropping every label of the instance",
			withoutReplace,
			"per container instance, but the metric_relabel_configs of prometheus.scrapeConfigs.kubernetes-nodes-cadvisor in " + valuesFile + " merge a restarted container's instances",
		},
		{
			"a scrape keeping cAdvisor's id",
			func(c *chart) {
				withoutReplace(c)
				for i, r := range c.cAdvisor.relabels {
					if r.Action == "labeldrop" {
						c.cAdvisor.relabels[i].Regex = "container_id|name|uid"
					}
				}
			},
			"",
		},
		{
			"a relabel action the check cannot apply",
			func(c *chart) {
				c.cAdvisor.relabels = append(slices.Clone(c.cAdvisor.relabels), relabel{Action: "hashmod", SourceLabels: []string{"id"}})
			},
			`cannot be read: cannot apply relabel action "hashmod"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadChart(t)
			tc.edit(&c)

			problems := check(c, []recommender.PromContract{oom})

			switch {
			case tc.want == "" && len(problems) != 0:
				t.Errorf("problems = %q, want none", problems)
			case tc.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], tc.want)):
				t.Errorf("problems = %q, want one containing %q", problems, tc.want)
			}
		})
	}
}
