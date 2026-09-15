package main

// Every HTTP answer carries a `cli_command`: the rx command that does what
// the request did, for a user to paste into a terminal. These tests hold
// the rendered string to that promise by handing it to the real command
// tree: a flag the CLI does not know, a value it cannot parse or a wrong
// number of arguments fails here instead of in a user's shell.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/internal/webapi"
)

// cliCommandCase is one case of the fixture shared with rx-python.
type cliCommandCase struct {
	Name     string         `json:"name"`
	Endpoint string         `json:"endpoint"`
	Params   map[string]any `json:"params"`
	Expected string         `json:"expected"`
}

// loadCLICommandCases reads testdata/cli-commands.json at the repo root.
func loadCLICommandCases(t *testing.T) []cliCommandCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cli-commands.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		Cases []cliCommandCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return fixture.Cases
}

// builderParams converts JSON-decoded fixture params to the Go types the
// handlers pass to webapi.BuildCLICommand: a JSON array becomes []string
// and a JSON number becomes *int. Strings and booleans pass unchanged.
func builderParams(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		switch typed := value.(type) {
		case []any:
			items := make([]string, 0, len(typed))
			for _, item := range typed {
				items = append(items, item.(string))
			}
			out[key] = items
		case float64:
			number := int(typed)
			out[key] = &number
		default:
			out[key] = value
		}
	}
	return out
}

// shellWords splits command into words the way a POSIX shell does, by
// asking one: `set --` makes the words the positional parameters and
// printf writes each one NUL-terminated. `set -f` turns off globbing, so
// an unquoted `*` would reach rx as itself rather than as file names.
func shellWords(t *testing.T, command string) []string {
	t.Helper()
	script := "set -f; set -- " + command + "; printf '%s\\0' \"$@\""
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("shell split %q: %v", command, err)
	}
	words := strings.Split(string(out), "\x00")
	return words[:len(words)-1] // printf ends the last word with a NUL too
}

// parseWithCommandTree resolves words (without the leading "rx") to a
// subcommand of the real command tree and parses its flags and arguments
// as `rx` would, without running the command.
func parseWithCommandTree(words []string) error {
	return parseWithRoot(newRootCmd(), words)
}

// parseWithRoot is parseWithCommandTree on a root the caller built, for
// a caller that first adds what cobra adds only inside Execute, such as
// the completion command.
func parseWithRoot(root *cobra.Command, words []string) error {
	sub, rest, err := root.Find(words)
	if err != nil {
		return err
	}
	if sub == root {
		return fmt.Errorf("no subcommand in %q", words)
	}
	// ParseFlags merges the root's persistent flags (--search-root,
	// --hidden) into the subcommand's flag set first, as Execute does.
	if err := sub.ParseFlags(rest); err != nil {
		return err
	}
	return sub.ValidateArgs(sub.Flags().Args())
}

// requireRunnableCommand fails the test unless command is an `rx`
// command line that the real command tree accepts.
func requireRunnableCommand(t *testing.T, command string) []string {
	t.Helper()
	words := shellWords(t, command)
	if len(words) < 2 || words[0] != "rx" {
		t.Fatalf("%q does not start with an rx subcommand", command)
	}
	if err := parseWithCommandTree(words[1:]); err != nil {
		t.Fatalf("%q is not a valid rx command: %v", command, err)
	}
	return words[1:]
}

func TestCLICommand_EveryFixtureCaseParsesWithTheRealCommandTree(t *testing.T) {
	for _, testCase := range loadCLICommandCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			rendered := webapi.BuildCLICommand(testCase.Endpoint, builderParams(testCase.Params))
			requireRunnableCommand(t, rendered)
		})
	}
}

// The parser has to be strict for the test above to mean anything.
func TestCLICommand_ParserRejectsAnUnknownFlagAndABadValue(t *testing.T) {
	for _, command := range []string{
		"rx samples /var/log/app.log --lines=1 --before-context=2",
		"rx compress /var/log/app.log --level=high",
		"rx samples /var/log/app.log /var/log/other.log --lines=1",
	} {
		if err := parseWithCommandTree(shellWords(t, command)[1:]); err == nil {
			t.Errorf("%q parsed without error", command)
		}
	}
}

// The table BuildCLICommand renders from names flags the command tree
// has, and what it treats as "rx without this flag" is the flag's own
// default there, so a default changed on one side fails here.
func TestCLICommand_TableMatchesTheCommandTree(t *testing.T) {
	root := newRootCmd()
	for name, op := range webapi.CLICommandOperations() {
		sub, _, err := root.Find([]string{op.Subcommand})
		if err != nil || sub == root {
			t.Errorf("%s: no subcommand %q", name, op.Subcommand)
			continue
		}
		fields := map[string]bool{}
		for _, arg := range op.Args {
			fields[arg.Field] = true
		}
		for _, arg := range op.Args {
			switch arg.Kind {
			case webapi.ArgPositional:
				continue
			case webapi.ArgSwitchList:
				for _, flag := range trace.MatchingFlags {
					if sub.Flags().Lookup(flag.Long) == nil {
						t.Errorf("%s: rx %s has no --%s", name, op.Subcommand, flag.Long)
					}
				}
				continue
			}
			flag := sub.Flags().Lookup(arg.Flag)
			switch {
			case flag == nil:
				t.Errorf("%s: rx %s has no --%s", name, op.Subcommand, arg.Flag)
			case arg.Absent.NoValue:
				// Leaving the flag out means something no value spells.
			case arg.Absent.SameAs != "":
				if !fields[arg.Absent.SameAs] {
					t.Errorf("%s: --%s defaults to field %q, which the operation lacks",
						name, arg.Flag, arg.Absent.SameAs)
				}
			case flag.DefValue != arg.Absent.Value:
				t.Errorf("%s: --%s defaults to %q in rx %s, the table says %q",
					name, arg.Flag, flag.DefValue, op.Subcommand, arg.Absent.Value)
			}
		}
		for _, word := range op.FixedFlags {
			if sub.Flags().Lookup(strings.TrimPrefix(word, "--")) == nil {
				t.Errorf("%s: rx %s has no %s", name, op.Subcommand, word)
			}
		}
	}
}
