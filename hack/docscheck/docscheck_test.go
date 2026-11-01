package docscheck

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/noony/k8s-sustain/cmd/controller"
	_ "github.com/noony/k8s-sustain/cmd/dashboard"
	_ "github.com/noony/k8s-sustain/cmd/webhook"
	"github.com/noony/k8s-sustain/hack/recordingrules"
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

// Every flag has a row, with its default, in its command's section of the
// CLI reference: a root persistent flag once, in the global one.
func TestCLIFlagsDocumented(t *testing.T) {
	const doc = "docs/reference/cli.md"
	for _, p := range checkCLI(readFile(t, doc), registeredFlags(controller.RootCmd())) {
		t.Errorf("%s: %s", doc, p)
	}
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

func TestRecordingRulesDocumented(t *testing.T) {
	rules, err := recordingrules.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	const doc = "docs/reference/recording-rules.md"
	assertDocumented(t, readFile(t, doc), doc, slices.Sorted(maps.Keys(rules)), backticked)
}

var subchartKeys = map[string]bool{"prometheus": true}

func chartValues(t *testing.T) (map[string]any, schemaNode) {
	t.Helper()
	return parseValues(t, readFile(t, "charts/k8s-sustain/values.yaml")),
		parseSchema(t, readFile(t, "charts/k8s-sustain/values.schema.json"))
}

// Every value of values.yaml or of its schema has a row, and a row's default
// is the one values.yaml holds.
func TestHelmValuesDocumented(t *testing.T) {
	values, schema := chartValues(t)
	const doc = "docs/reference/helm-values.md"
	text := readFile(t, doc)
	assertDocumented(t, text, doc, helmValueNames(values, schema, subchartKeys), backticked)
	for _, p := range checkHelmDefaults(text, values) {
		t.Errorf("%s: %s", doc, p)
	}
}

// A flag the chart renders from a value exists, its default is a value the
// schema accepts, and a value rendered only when set defaults to it.
func TestChartRendersFlagDefaults(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, "charts/k8s-sustain/templates/*"))
	if err != nil {
		t.Fatal(err)
	}
	templates := map[string]string{}
	for _, f := range files {
		rel, _ := filepath.Rel(repoRoot, f)
		templates[filepath.Base(f)] = readFile(t, rel)
	}
	rendered := renderedFlags(templates)
	if len(rendered) == 0 {
		t.Fatal("found no flag rendered from a value")
	}
	values, schema := chartValues(t)
	for _, p := range checkRenderedDefaults(controller.RootCmd(), rendered, values, schema) {
		t.Error(p)
	}
}
