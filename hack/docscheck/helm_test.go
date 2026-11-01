package docscheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

type schemaNode struct {
	Ref         string                `json:"$ref"`
	Type        any                   `json:"type"`
	Pattern     string                `json:"pattern"`
	Enum        []any                 `json:"enum"`
	Minimum     *float64              `json:"minimum"`
	Maximum     *float64              `json:"maximum"`
	Properties  map[string]schemaNode `json:"properties"`
	Definitions map[string]schemaNode `json:"definitions"`
}

func parseSchema(t *testing.T, raw string) schemaNode {
	t.Helper()
	var root schemaNode
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		t.Fatal(err)
	}
	return root
}

// parseValues decodes a values file keeping numbers as written, so 10000000
// stays 10000000 rather than 1e+07.
func parseValues(t *testing.T, raw string) map[string]any {
	t.Helper()
	j, err := yaml.YAMLToJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		t.Fatal(err)
	}
	return values
}

func valueAt(values map[string]any, path string) (any, bool) {
	var cur any = values
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func (root schemaNode) resolve(n schemaNode) schemaNode {
	if name, ok := strings.CutPrefix(n.Ref, "#/definitions/"); ok {
		return root.Definitions[name]
	}
	return n
}

// at is the schema of the value at path, a reference left unresolved.
func (root schemaNode) at(path string) (schemaNode, bool) {
	n := root
	for _, k := range strings.Split(path, ".") {
		child, ok := root.resolve(n).Properties[k]
		if !ok {
			return schemaNode{}, false
		}
		n = child
	}
	return n, true
}

func (n schemaNode) types() []string {
	switch t := n.Type.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, v := range t {
			out = append(out, fmt.Sprint(v))
		}
		return out
	}
	return nil
}

// accepts is the subset of JSON Schema the chart's flag-rendering values use.
func (n schemaNode) accepts(v any) error {
	var typ string
	switch v.(type) {
	case bool:
		typ = "boolean"
	case int64:
		typ = "integer"
	case string:
		typ = "string"
	}
	if ts := n.types(); len(ts) > 0 && !slices.Contains(ts, typ) && (typ != "integer" || !slices.Contains(ts, "number")) {
		return fmt.Errorf("is not of type %s", strings.Join(ts, "|"))
	}
	if s, ok := v.(string); ok && n.Pattern != "" {
		if ok, err := regexp.MatchString(n.Pattern, s); err != nil || !ok {
			return fmt.Errorf("does not match %s", n.Pattern)
		}
	}
	if len(n.Enum) > 0 && !slices.ContainsFunc(n.Enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) }) {
		return fmt.Errorf("is not one of %v", n.Enum)
	}
	if i, ok := v.(int64); ok {
		if n.Minimum != nil && float64(i) < *n.Minimum || n.Maximum != nil && float64(i) > *n.Maximum {
			return fmt.Errorf("is out of range")
		}
	}
	return nil
}

// renderedFlag is a flag a template renders from one value.
type renderedFlag struct {
	template string
	// command is the subcommand the template's args run, empty in a helper.
	command string
	flag    string
	value   string
	// whenSet is a flag rendered only when its value is set: otherwise the
	// binary's default applies, so the value's default must be that default.
	whenSet bool
}

var (
	argFromValueRe = regexp.MustCompile(`^\s*- --([a-z0-9-]+)=\{\{-?\s*(?:int64\s+)?\$?[A-Za-z]*\.Values\.([A-Za-z0-9_.]+)\s*-?\}\}\s*$`)
	argFromDotRe   = regexp.MustCompile(`^\s*- --([a-z0-9-]+)=\{\{-?\s*(?:int64\s+)?\.\s*-?\}\}\s*$`)
	argTrueRe      = regexp.MustCompile(`^\s*- --([a-z0-9-]+)=true\s*$`)
	conditionRe    = regexp.MustCompile(`\{\{-?\s*(if|with)\s+(\S+)`)
	valuesPathRe   = regexp.MustCompile(`^\$?[A-Za-z]*\.Values\.([A-Za-z0-9_.]+)$`)
	argsCommandRe  = regexp.MustCompile(`(?m)^\s*args:\s*\n\s*- ([a-z]+)\s*$`)
)

// renderedFlags finds every flag a template renders straight from a value:
// `--flag={{ .Values.x }}`, `--flag={{ . }}` under `with .Values.x`, and
// `--flag=true` under `if .Values.x`. A flag built by a helper is not one.
func renderedFlags(templates map[string]string) []renderedFlag {
	var out []renderedFlag
	for name, src := range templates {
		var command string
		if m := argsCommandRe.FindStringSubmatch(src); m != nil {
			command = m[1]
		}
		var cond, condPath string
		for _, line := range strings.Split(src, "\n") {
			if m := conditionRe.FindStringSubmatch(line); m != nil {
				cond, condPath = m[1], ""
				if p := valuesPathRe.FindStringSubmatch(m[2]); p != nil {
					condPath = p[1]
				}
				continue
			}
			r := renderedFlag{template: name, command: command}
			switch m := argFromValueRe.FindStringSubmatch(line); {
			case m != nil:
				r.flag, r.value = m[1], m[2]
			case cond == "with" && condPath != "" && argFromDotRe.MatchString(line):
				r.flag, r.value, r.whenSet = argFromDotRe.FindStringSubmatch(line)[1], condPath, true
			case cond == "if" && condPath != "" && argTrueRe.MatchString(line):
				r.flag, r.value, r.whenSet = argTrueRe.FindStringSubmatch(line)[1], condPath, true
			default:
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

func lookupFlag(root *cobra.Command, command, name string) *pflag.Flag {
	if f := root.PersistentFlags().Lookup(name); f != nil {
		return f
	}
	for _, sub := range root.Commands() {
		if sub.Name() == command && command != "" {
			return sub.LocalNonPersistentFlags().Lookup(name)
		}
	}
	return nil
}

// typedDefault is a flag's default as the value rendering it would hold it.
func typedDefault(f *pflag.Flag) (any, bool) {
	switch f.Value.Type() {
	case "bool":
		return f.DefValue == "true", true
	case "int", "int64":
		i, err := strconv.ParseInt(f.DefValue, 10, 64)
		return i, err == nil
	case "string", "duration":
		return f.DefValue, true
	}
	return nil, false
}

// checkRenderedDefaults reports a flag a template renders that its command
// does not register, a value whose schema rejects the flag's default (so the
// binary's own default could not be set through the chart), and a value
// rendered only when set whose default is not the flag's.
func checkRenderedDefaults(root *cobra.Command, rendered []renderedFlag, values map[string]any, schema schemaNode) []string {
	var problems []string
	for _, r := range rendered {
		f := lookupFlag(root, r.command, r.flag)
		if f == nil {
			problems = append(problems, fmt.Sprintf("%s renders --%s, which %q does not register", r.template, r.flag, r.command))
			continue
		}
		node, ok := schema.at(r.value)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s renders --%s from %s, which the values schema does not describe", r.template, r.flag, r.value))
		} else if def, ok := typedDefault(f); ok {
			if err := schema.resolve(node).accepts(def); err != nil {
				problems = append(problems, fmt.Sprintf("%s: the schema of %s rejects the --%s default %q: it %v", r.template, r.value, r.flag, f.DefValue, err))
			}
		}
		if !r.whenSet {
			continue
		}
		v, ok := valueAt(values, r.value)
		if !ok || !sameDefault(f, fmt.Sprint(v)) {
			problems = append(problems, fmt.Sprintf("%s renders --%s only when %s is set, so its default %v must be the flag's %q", r.template, r.flag, r.value, v, f.DefValue))
		}
	}
	sort.Strings(problems)
	return problems
}

// helmValueNames is every value the chart takes: a leaf of values.yaml or of
// the schema. values.yaml is walked down to where the schema stops, so a
// value the schema describes as a whole (resources, a probe) is one name.
func helmValueNames(values map[string]any, schema schemaNode, skip map[string]bool) []string {
	names := map[string]struct{}{}
	var walkSchema func(schemaNode, string)
	walkSchema = func(n schemaNode, path string) {
		if len(n.Properties) == 0 {
			names[path] = struct{}{}
			return
		}
		for k, child := range n.Properties {
			walkSchema(child, path+"."+k)
		}
	}
	var walkValues func(any, string)
	walkValues = func(v any, path string) {
		m, ok := v.(map[string]any)
		node, described := schema.at(path)
		if !ok || len(m) == 0 || described && (node.Ref != "" || len(node.Properties) == 0) {
			names[path] = struct{}{}
			return
		}
		for k, child := range m {
			walkValues(child, path+"."+k)
		}
	}
	for k, child := range schema.Properties {
		if !skip[k] {
			walkSchema(child, k)
		}
	}
	for k, child := range values {
		if !skip[k] {
			walkValues(child, k)
		}
	}
	return uniqueSorted(names)
}

var helmRowRe = regexp.MustCompile("(?m)^\\| `([A-Za-z0-9_.]+)` \\| ([^|]*) \\|")

// checkHelmDefaults reports a row whose default is one backticked value that
// differs from values.yaml.
func checkHelmDefaults(doc string, values map[string]any) []string {
	var problems []string
	for _, r := range helmRowRe.FindAllStringSubmatch(doc, -1) {
		m := backtickedRe.FindStringSubmatch(strings.TrimSpace(r[2]))
		v, ok := valueAt(values, r[1])
		if m == nil || !ok {
			continue
		}
		if !bytes.Equal(writtenJSON(m[1]), mustJSON(v)) {
			problems = append(problems, fmt.Sprintf("`%s`: the row says %s, values.yaml says %s", r[1], strings.TrimSpace(r[2]), mustJSON(v)))
		}
	}
	sort.Strings(problems)
	return problems
}

// writtenJSON reads a default as YAML, the way values.yaml would hold it, and
// as a plain string when it is not YAML (`:8080`).
func writtenJSON(written string) []byte {
	j, err := yaml.YAMLToJSON([]byte(written))
	if err != nil {
		return mustJSON(written)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return mustJSON(written)
	}
	return mustJSON(v)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestRenderedDefaultsCheckCatchesEveryDrift(t *testing.T) {
	root := &cobra.Command{Use: "k8s-sustain"}
	root.PersistentFlags().Bool("recommend-only", false, "")
	start := &cobra.Command{Use: "start"}
	start.Flags().Int("max", 8, "")
	start.Flags().Duration("interval", 5*time.Minute, "")
	start.Flags().String("level", "info", "")
	root.AddCommand(start)

	templates := map[string]string{
		"deployment.yaml": strings.Join([]string{
			"args:",
			"  - start",
			"  {{- with .Values.max }}",
			"  - --max={{ int64 . }}",
			"  {{- end }}",
			"  - --interval={{ .Values.interval }}",
			"  - --level={{ .Values.level }}",
			"  - --unknown={{ .Values.level }}",
			"  - --literal=/tls/tls.crt",
		}, "\n"),
		"_helpers.tpl": strings.Join([]string{
			"{{- if $ctx.Values.recommendOnly }}",
			"- --recommend-only=true",
			"{{- end }}",
			"{{- if $tls.insecure }}",
			"- --insecure=true",
			"{{- end }}",
		}, "\n"),
	}
	values := parseValues(t, "max: 4\ninterval: 5m\nlevel: error\nrecommendOnly: true\n")
	schema := parseSchema(t, `{"properties": {
		"max": {"type": "integer"},
		"interval": {"type": "string", "pattern": "^[0-9]+(s|m|h)$"},
		"level": {"type": "string", "enum": ["warn", "error"]},
		"recommendOnly": {"type": "boolean"}
	}}`)

	want := []string{
		`_helpers.tpl renders --recommend-only only when recommendOnly is set, so its default true must be the flag's "false"`,
		`deployment.yaml renders --max only when max is set, so its default 4 must be the flag's "8"`,
		`deployment.yaml renders --unknown, which "start" does not register`,
		`deployment.yaml: the schema of interval rejects the --interval default "5m0s": it does not match ^[0-9]+(s|m|h)$`,
		`deployment.yaml: the schema of level rejects the --level default "info": it is not one of [warn error]`,
	}
	got := checkRenderedDefaults(root, renderedFlags(templates), values, schema)
	if !slices.Equal(got, want) {
		t.Errorf("problems =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestHelmValueNamesComeFromValuesAndSchema(t *testing.T) {
	values := parseValues(t, strings.Join([]string{
		"controller:",
		"  replicaCount: 1",
		"  undescribed: true",
		"  resources:",
		"    requests: {cpu: 100m}",
		"  annotations: {}",
		"prometheus:",
		"  enabled: true",
	}, "\n"))
	schema := parseSchema(t, `{"properties": {"controller": {"properties": {
		"replicaCount": {"type": "integer"},
		"resources": {"$ref": "#/definitions/resources"},
		"terminationGracePeriodSeconds": {"type": "integer"}
	}}}, "definitions": {"resources": {"type": "object", "properties": {"requests": {"type": "object"}}}}}`)

	got := helmValueNames(values, schema, map[string]bool{"prometheus": true})
	want := []string{
		"controller.annotations",
		"controller.replicaCount",
		"controller.resources",
		"controller.terminationGracePeriodSeconds",
		"controller.undescribed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestHelmDefaultsCheckCatchesADrift(t *testing.T) {
	values := parseValues(t, "controller:\n  reconcileInterval: 5m\n  replicaCount: 1\n  hosts: [a.local]\n  name: \"\"\n")
	doc := strings.Join([]string{
		"| `controller.reconcileInterval` | `10m` | d |",
		"| `controller.replicaCount` | `1` | d |",
		"| `controller.hosts` | `[a.local]` | d |",
		"| `controller.name` | `\"\"` | d |",
		"| `controller.resources` | see below | d |",
		"| `controller.terminationGracePeriodSeconds` | `30` | d |",
	}, "\n")
	want := []string{"`controller.reconcileInterval`: the row says `10m`, values.yaml says \"5m\""}
	if got := checkHelmDefaults(doc, values); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}
