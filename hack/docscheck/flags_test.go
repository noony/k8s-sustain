package docscheck

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandFlags is the flags one command registers itself: the root's
// persistent flags under the name "", a subcommand's own under its name.
type commandFlags struct {
	name  string
	flags []*pflag.Flag
}

func visibleFlags(fs *pflag.FlagSet) []*pflag.Flag {
	var out []*pflag.Flag
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && f.Name != "version" {
			out = append(out, f)
		}
	})
	return out
}

func registeredFlags(root *cobra.Command) []commandFlags {
	out := []commandFlags{{name: "", flags: visibleFlags(root.PersistentFlags())}}
	for _, sub := range root.Commands() {
		if fs := visibleFlags(sub.LocalNonPersistentFlags()); len(fs) > 0 {
			out = append(out, commandFlags{name: sub.Name(), flags: fs})
		}
	}
	return out
}

var (
	cliSectionRe    = regexp.MustCompile(`(?m)^## (.+)$`)
	cliSubcommandRe = regexp.MustCompile("^`k8s-sustain ([a-z]+)`$")
	cliFlagRowRe    = regexp.MustCompile("(?m)^\\| `--([a-z0-9-]+)` \\| ([^|]*) \\|")
	backtickedRe    = regexp.MustCompile("^`([^`]*)`$")
)

// cliSections maps each section of the CLI reference that documents flags to
// its rows, flag name to default cell: "Global flags" to "", a
// "`k8s-sustain <subcommand>`" section to the subcommand.
func cliSections(doc string) map[string]map[string]string {
	out := map[string]map[string]string{}
	headings := cliSectionRe.FindAllStringSubmatchIndex(doc, -1)
	for i, h := range headings {
		title := strings.TrimSpace(doc[h[2]:h[3]])
		var name string
		switch m := cliSubcommandRe.FindStringSubmatch(title); {
		case title == "Global flags":
		case m != nil:
			name = m[1]
		default:
			continue
		}
		end := len(doc)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		rows := map[string]string{}
		for _, r := range cliFlagRowRe.FindAllStringSubmatch(doc[h[1]:end], -1) {
			rows[r[1]] = strings.TrimSpace(r[2])
		}
		out[name] = rows
	}
	return out
}

func documentedDefault(cell string) (string, bool) {
	switch cell {
	case "", "—", "*(empty)*":
		return "", true
	}
	if m := backtickedRe.FindStringSubmatch(cell); m != nil {
		return m[1], true
	}
	return "", false
}

// flagDefault is the flag's default as a user writes it: an empty list is
// empty, like an empty string.
func flagDefault(f *pflag.Flag) string {
	if f.DefValue == "[]" {
		return ""
	}
	return f.DefValue
}

// sameDefault compares a written value with a flag's default, reading a
// duration as a duration: 5m is the 5m0s pflag prints.
func sameDefault(f *pflag.Flag, written string) bool {
	def := flagDefault(f)
	if f.Value.Type() == "duration" && written != "" {
		got, err := time.ParseDuration(written)
		want, _ := time.ParseDuration(def)
		return err == nil && got == want
	}
	return written == def
}

// checkCLI reports every way doc disagrees with the commands' flags: a flag
// without a row in its command's section (the root's persistent flags in the
// global one, documented once), a row for a flag the command does not
// register, and a default that is not the flag's.
func checkCLI(doc string, cmds []commandFlags) []string {
	sections := cliSections(doc)
	var problems []string
	for _, c := range cmds {
		label := "the global flags"
		if c.name != "" {
			label = "`k8s-sustain " + c.name + "`"
		}
		rows, ok := sections[c.name]
		if !ok {
			problems = append(problems, fmt.Sprintf("no section documents %s", label))
			continue
		}
		registered := map[string]bool{}
		for _, f := range c.flags {
			registered[f.Name] = true
			cell, ok := rows[f.Name]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: no row for --%s", label, f.Name))
				continue
			}
			if written, ok := documentedDefault(cell); !ok || !sameDefault(f, written) {
				problems = append(problems, fmt.Sprintf("%s: --%s defaults to %q, the row says %s", label, f.Name, f.DefValue, cell))
			}
		}
		for name := range rows {
			if !registered[name] {
				problems = append(problems, fmt.Sprintf("%s: documents --%s, which it does not register", label, name))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func seededCommand() *cobra.Command {
	root := &cobra.Command{Use: "k8s-sustain"}
	root.PersistentFlags().String("shared", "x", "")
	root.PersistentFlags().StringSlice("namespaces", nil, "")
	run := &cobra.Command{Use: "run"}
	run.Flags().Duration("interval", 5*time.Minute, "")
	run.Flags().String("name", "", "")
	root.AddCommand(run, &cobra.Command{Use: "version"})
	return root
}

func TestCLICheckAcceptsAgreeingDocs(t *testing.T) {
	doc := "## Global flags\n\n| Flag | Default | Description |\n|---|---|---|\n" +
		"| `--shared` | `x` | d |\n| `--namespaces` | — | d |\n\n" +
		"## `k8s-sustain run`\n\n| `--interval` | `5m` | d |\n| `--name` | *(empty)* | d |\n\n" +
		"## Environment variables\n\n| `--elsewhere` | `y` | d |\n"
	if problems := checkCLI(doc, registeredFlags(seededCommand())); len(problems) > 0 {
		t.Errorf("problems = %q, want none", problems)
	}
}

func TestCLICheckCatchesEveryDrift(t *testing.T) {
	doc := "## Global flags\n\n| `--shared` | `y` | d |\n| `--namespaces` | — | d |\n\n" +
		"## `k8s-sustain run`\n\n| `--interval` | `10m` | d |\n| `--shared` | `x` | d |\n"
	want := []string{
		"the global flags: --shared defaults to \"x\", the row says `y`",
		"`k8s-sustain run`: --interval defaults to \"5m0s\", the row says `10m`",
		"`k8s-sustain run`: documents --shared, which it does not register",
		"`k8s-sustain run`: no row for --name",
	}
	sort.Strings(want)
	if got := checkCLI(doc, registeredFlags(seededCommand())); !slices.Equal(got, want) {
		t.Errorf("problems =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestCLICheckWantsASectionPerCommand(t *testing.T) {
	doc := "## Global flags\n\n| `--shared` | `x` | d |\n| `--namespaces` | — | d |\n"
	want := []string{"no section documents `k8s-sustain run`"}
	if got := checkCLI(doc, registeredFlags(seededCommand())); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}
