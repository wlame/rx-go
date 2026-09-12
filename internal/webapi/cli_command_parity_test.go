package webapi

// `cli_command` is the string the viewer shows a user so they can
// reproduce a request from a terminal, and the two backends rendered it
// differently for the same request: rx-go put the path last and wrote
// `--lines 2`, rx-python put the path first and wrote `-l 2`. One of the
// two also taught a flag style the project asks people not to use.
//
// The rendering is now `rx <subcommand> <positionals...> <--long=value...>`,
// and the cases live in a fixture both repos read — testdata/
// cli-commands.json here, tests/data/cli-commands.json in rx-python. The
// two files are copies and must be edited together, which is the point:
// a change on one side that is not made on the other fails there.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/trace"
)

type cliCommandCase struct {
	Name     string         `json:"name"`
	Endpoint string         `json:"endpoint"`
	Params   map[string]any `json:"params"`
	Expected string         `json:"expected"`
}

type cliCommandFixture struct {
	Cases []cliCommandCase `json:"cases"`
}

// toBuilderParams converts the JSON-decoded params into the types
// BuildCLICommand expects. JSON gives []any and float64 where the
// handlers pass []string and *int, so the conversion happens here rather
// than by loosening the builder for the sake of a test.
func toBuilderParams(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		switch typed := value.(type) {
		case []any:
			strings := make([]string, 0, len(typed))
			for _, item := range typed {
				strings = append(strings, item.(string))
			}
			out[key] = strings
		case float64:
			number := int(typed)
			out[key] = &number
		default:
			out[key] = value
		}
	}
	return out
}

func TestBuildCLICommand_MatchesTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cli-commands.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture cliCommandFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			got := BuildCLICommand(testCase.Endpoint, toBuilderParams(testCase.Params))
			if got != testCase.Expected {
				t.Errorf("got  %q\nwant %q", got, testCase.Expected)
			}
		})
	}
}

// Every long flag in a rendered command uses --flag=value. A space form
// is what rx-go used to emit and what the project's own CLI docs were
// corrected away from.
func TestBuildCLICommand_UsesTheEqualsFormForEveryLongFlag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cli-commands.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture cliCommandFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	// A long flag that takes a value must carry it after an '=' in the
	// same word. Boolean flags (--force, --info) carry none, so the
	// check is on the word after a valueless flag: it must be another
	// flag or nothing.
	valuelessFlags := map[string]bool{
		"--force": true, "--analyze": true, "--info": true, "--json": true,
	}
	for _, flag := range trace.MatchingFlags {
		valuelessFlags["--"+flag.Long] = true
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			words := splitCommandWords(BuildCLICommand(testCase.Endpoint, toBuilderParams(testCase.Params)))
			for i, word := range words {
				if len(word) < 2 || word[:2] != "--" {
					continue
				}
				if valuelessFlags[word] {
					continue
				}
				if !containsByte(word, '=') {
					t.Errorf("%q takes its value in the next word (%q); use --flag=value",
						word, nextWord(words, i))
				}
			}
		})
	}
}

func splitCommandWords(command string) []string {
	words := []string{}
	current := ""
	for i := 0; i < len(command); i++ {
		if command[i] == ' ' {
			if current != "" {
				words = append(words, current)
				current = ""
			}
			continue
		}
		current += string(command[i])
	}
	if current != "" {
		words = append(words, current)
	}
	return words
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func nextWord(words []string, i int) string {
	if i+1 < len(words) {
		return words[i+1]
	}
	return ""
}
