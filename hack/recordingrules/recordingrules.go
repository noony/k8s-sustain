// Package recordingrules reads the recording rules the operator chart defines,
// for the tests under hack/ that check the code and the docs against them.
package recordingrules

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// ValuesFile defines the rules, under prometheusRule.groups, for both the
// bundled Prometheus and the standalone PrometheusRule.
const ValuesFile = "charts/k8s-sustain/values.yaml"

// Rule is one definition of a recording rule: several may record one name.
type Rule struct {
	Group    string
	Interval string
	Expr     string
}

// Load reads the rules of the chart in the repository at root, by the name
// each records.
func Load(root string) (map[string][]Rule, error) {
	b, err := os.ReadFile(filepath.Join(root, ValuesFile)) //nolint:gosec // G304: root is the repository the hack tests check.
	if err != nil {
		return nil, err
	}
	var v struct {
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
	}
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", ValuesFile, err)
	}
	rules := map[string][]Rule{}
	for _, g := range v.PrometheusRule.Groups {
		for _, r := range g.Rules {
			if r.Record != "" {
				rules[r.Record] = append(rules[r.Record], Rule{Group: g.Name, Interval: g.Interval, Expr: r.Expr})
			}
		}
	}
	return rules, nil
}
