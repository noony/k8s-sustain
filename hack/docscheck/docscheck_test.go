package docscheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/noony/k8s-sustain/cmd/controller"
	_ "github.com/noony/k8s-sustain/cmd/dashboard"
	_ "github.com/noony/k8s-sustain/cmd/webhook"
)

const repoRoot = "../.."

func readFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func assertDocumented(t *testing.T, doc, docPath string, names []string, format func(string) string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatalf("found nothing to check against %s", docPath)
	}
	var missing []string
	for _, n := range names {
		if !strings.Contains(doc, format(n)) {
			missing = append(missing, format(n))
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s is missing %d entries:\n  %s", docPath, len(missing), strings.Join(missing, "\n  "))
	}
}

func uniqueSorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func backticked(s string) string { return "`" + s + "`" }

func TestCLIFlagsDocumented(t *testing.T) {
	names := map[string]struct{}{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		add := func(f *pflag.Flag) {
			if f.Name != "help" {
				names[f.Name] = struct{}{}
			}
		}
		c.Flags().VisitAll(add)
		c.PersistentFlags().VisitAll(add)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(controller.RootCmd())

	const doc = "docs/reference/cli.md"
	assertDocumented(t, readFile(t, doc), doc, uniqueSorted(names), func(n string) string { return "`--" + n + "`" })
}

var metricNameRe = regexp.MustCompile(`Name:\s*"(k8s_sustain_[a-z0-9_]+)"`)

func TestMetricsDocumented(t *testing.T) {
	names := map[string]struct{}{}
	for _, dir := range []string{"internal/controller", "internal/webhook", "internal/dashboard"} {
		files, err := filepath.Glob(filepath.Join(repoRoot, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range metricNameRe.FindAllStringSubmatch(string(b), -1) {
				names[m[1]] = struct{}{}
			}
		}
	}

	const doc = "docs/reference/metrics.md"
	assertDocumented(t, readFile(t, doc), doc, uniqueSorted(names), backticked)
}

var recordRe = regexp.MustCompile(`(?m)^\s*-?\s*record:\s*(\S+)\s*$`)

func TestRecordingRulesDocumented(t *testing.T) {
	names := map[string]struct{}{}
	for _, m := range recordRe.FindAllStringSubmatch(readFile(t, "charts/k8s-sustain/values.yaml"), -1) {
		names[m[1]] = struct{}{}
	}

	const doc = "docs/reference/recording-rules.md"
	assertDocumented(t, readFile(t, doc), doc, uniqueSorted(names), backticked)
}

type schemaNode struct {
	Properties map[string]schemaNode `json:"properties"`
}

var subchartKeys = map[string]bool{"prometheus": true}

func TestHelmValuesDocumented(t *testing.T) {
	var root schemaNode
	if err := json.Unmarshal([]byte(readFile(t, "charts/k8s-sustain/values.schema.json")), &root); err != nil {
		t.Fatal(err)
	}

	names := map[string]struct{}{}
	var walk func(schemaNode, string)
	walk = func(n schemaNode, prefix string) {
		for k, child := range n.Properties {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if len(child.Properties) == 0 {
				names[path] = struct{}{}
				continue
			}
			walk(child, path)
		}
	}
	for k, child := range root.Properties {
		if subchartKeys[k] {
			continue
		}
		walk(schemaNode{Properties: map[string]schemaNode{k: child}}, "")
	}

	const doc = "docs/reference/helm-values.md"
	assertDocumented(t, readFile(t, doc), doc, uniqueSorted(names), backticked)
}
