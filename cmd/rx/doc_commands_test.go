package main

// The README and the pages under docs/ show rx command lines for a reader
// to copy. These tests hand every one of them to the real command tree,
// so a renamed flag, a wrong short flag or a value the flag cannot parse
// fails here instead of in a reader's shell.
//
// A command is a line inside a ```bash, ```sh, ```shell or ```console
// fence whose command word is `rx`, after an optional `$ ` prompt,
// leading `NAME=value` assignments and `time`. A line ending in `\`
// continues on the next one. A pipe, a list operator, a redirection or
// a comment ends the command. A line where rx is not the command word —
// `find … -exec rx index {} \;`, `sudo -u u env … rx serve` — is not
// checked.

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// docCommand is one rx command line found in a documentation page.
type docCommand struct {
	Location string   // file:line of the line that starts the command
	Words    []string // the command split into words, "rx" first
}

// shellFenceLanguages are the fence info strings whose lines are shell
// input. Other fences hold output, JSON or Go and are not read.
var shellFenceLanguages = map[string]bool{"bash": true, "sh": true, "shell": true, "console": true}

// commandPrefixWords may stand before `rx` on a line without changing
// what rx parses.
var commandPrefixWords = map[string]bool{"time": true}

// docPages returns README.md and every Markdown page under docs/, as
// paths relative to this package's directory.
func docPages(t *testing.T) []string {
	t.Helper()
	return docPagesUnder(t, filepath.Join("..", ".."))
}

// unpublishedDocDirs are the directories right under docs/ that
// .gitignore leaves out: working material that exists on one machine
// only, which mkdocs does not publish either. A page there is no doc
// page, and the docs tests do not read it.
var unpublishedDocDirs = map[string]bool{"plans": true, "superpowers": true}

// docPagesUnder returns README.md and every Markdown page under docs/
// of the repository at repoRoot, outside unpublishedDocDirs.
func docPagesUnder(t *testing.T, repoRoot string) []string {
	t.Helper()
	docsDir := filepath.Join(repoRoot, "docs")
	pages := []string{filepath.Join(repoRoot, "README.md")}
	err := filepath.WalkDir(docsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// filepath.SkipDir, returned for a directory, makes WalkDir
		// leave out everything below it.
		if entry.IsDir() && filepath.Dir(path) == docsDir && unpublishedDocDirs[entry.Name()] {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".md") {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	return pages
}

// shellFenceLines returns the lines inside shell fences of page, with
// continuation lines joined, each with the line number it starts on.
func shellFenceLines(t *testing.T, page string) map[int]string {
	t.Helper()
	file, err := os.Open(page)
	if err != nil {
		t.Fatalf("open %s: %v", page, err)
	}
	defer func() { _ = file.Close() }()

	lines := map[int]string{}
	inFence, inShellFence := false, false
	pending, pendingStart := "", 0
	scanner := bufio.NewScanner(file)
	for number := 1; scanner.Scan(); number++ {
		trimmed := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(trimmed, "```") {
			inShellFence = !inFence && shellFenceLanguages[strings.TrimPrefix(trimmed, "```")]
			inFence = !inFence
			continue
		}
		if !inShellFence {
			continue
		}
		if pending == "" {
			pendingStart = number
		}
		if continued, ok := strings.CutSuffix(trimmed, `\`); ok && !strings.HasSuffix(continued, `\`) {
			pending += continued + " "
			continue
		}
		lines[pendingStart] = pending + trimmed
		pending = ""
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	return lines
}

// splitCommandWords splits line into words the way a POSIX shell quotes
// them, without expanding anything: a `$VAR` or `$(…)` stays as written
// and nothing runs. It stops at the first unquoted pipe, list operator,
// redirection or comment, since what follows is not part of the command.
func splitCommandWords(line string) []string {
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	for i := 0; i < len(line); i++ {
		char := line[i]
		switch {
		case char == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				end = len(line) - i - 1
			}
			word.WriteString(line[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case char == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) && strings.IndexByte(`"\$`+"`", line[i+1]) >= 0 {
					i++
				}
				word.WriteByte(line[i])
			}
		case char == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case char == ' ' || char == '\t':
			endWord()
		case (char == '<' || char == '>') && isDescriptorNumber(word.String()):
			return words // `2>>log`: the 2 names a descriptor, not an argument
		case strings.IndexByte("|;&<>", char) >= 0, char == '#' && !inWord:
			endWord()
			return words
		default:
			word.WriteByte(char)
			inWord = true
		}
	}
	endWord()
	return words
}

// isDescriptorNumber reports whether a word written right before `<` or
// `>` is a file descriptor number, as the 2 in `2>>log` is.
func isDescriptorNumber(word string) bool {
	return len(word) == 1 && word[0] >= '0' && word[0] <= '9'
}

// rxCommandWords returns the words of line from "rx" on, or nil when rx
// is not the command word of line.
func rxCommandWords(line string) []string {
	words := splitCommandWords(strings.TrimPrefix(line, "$ "))
	for len(words) > 0 && (isAssignment(words[0]) || commandPrefixWords[words[0]]) {
		words = words[1:]
	}
	if len(words) == 0 || words[0] != "rx" {
		return nil
	}
	return words
}

// isAssignment reports whether word is a shell `NAME=value` assignment.
func isAssignment(word string) bool {
	name, _, found := strings.Cut(word, "=")
	if !found || name == "" {
		return false
	}
	for i, char := range name {
		isLetter := char == '_' || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z')
		if !isLetter && (i == 0 || char < '0' || char > '9') {
			return false
		}
	}
	return true
}

// documentedCommands returns every rx command in the README and docs/.
func documentedCommands(t *testing.T) []docCommand {
	t.Helper()
	var commands []docCommand
	for _, page := range docPages(t) {
		for number, line := range shellFenceLines(t, page) {
			if words := rxCommandWords(line); words != nil {
				location := fmt.Sprintf("%s:%d", strings.TrimPrefix(page, "../../"), number)
				commands = append(commands, docCommand{Location: location, Words: words})
			}
		}
	}
	return commands
}

// parseDocumentedCommand parses words (without the leading "rx") as the
// rx binary would: a bare pattern routes to trace, and `--version` or
// `--help` alone is the root command's.
func parseDocumentedCommand(words []string) error {
	args := preprocessArgs(words)
	if len(args) == 1 && (args[0] == "--version" || args[0] == "--help") {
		return nil
	}
	root := newRootCmd()
	root.InitDefaultCompletionCmd() // Execute adds `rx completion`; Find alone does not
	if err := parseWithRoot(root, args); err != nil {
		return err
	}
	return longFlagsUseEquals(root, args)
}

// longFlagsUseEquals fails on a long flag that takes a value and is
// written with the value as the next word (`--port 8080`): the docs
// write `--port=8080`, as the help text and cli_command do.
func longFlagsUseEquals(root *cobra.Command, args []string) error {
	sub, rest, err := root.Find(args)
	if err != nil {
		return err
	}
	for _, word := range rest {
		name, isLong := strings.CutPrefix(word, "--")
		if !isLong || name == "" || strings.Contains(name, "=") {
			continue
		}
		flag := sub.Flags().Lookup(name)
		if flag == nil {
			flag = sub.InheritedFlags().Lookup(name)
		}
		if flag != nil && flag.NoOptDefVal == "" {
			return fmt.Errorf("--%s takes a value: write --%s=VALUE", name, name)
		}
	}
	return nil
}

func TestDocCommands_EveryDocumentedCommandParses(t *testing.T) {
	commands := documentedCommands(t)
	readmeCommands := 0
	for _, command := range commands {
		if strings.HasPrefix(command.Location, "README.md:") {
			readmeCommands++
		}
		if err := parseDocumentedCommand(command.Words[1:]); err != nil {
			t.Errorf("%s: %q: %v", command.Location, strings.Join(command.Words, " "), err)
		}
	}
	// An extractor that finds nothing would pass everything.
	if len(commands) < 50 || readmeCommands == 0 {
		t.Fatalf("found %d documented commands, %d in README.md; the extractor is broken",
			len(commands), readmeCommands)
	}
}

// The checks have to reject what they are there to reject.
func TestDocCommands_ChecksRejectWrongCommands(t *testing.T) {
	for _, line := range []string{
		`rx samples /var/log/app.log --lines=1 -C 3`,
		`rx serve --port 8080`,
		`rx compress /var/log/app.log --level=high`,
	} {
		words := rxCommandWords(line)
		if words == nil {
			t.Errorf("%q: not recognized as an rx command", line)
			continue
		}
		if err := parseDocumentedCommand(words[1:]); err == nil {
			t.Errorf("%q parsed without error", line)
		}
	}
}

func TestDocCommands_SplitsLikeAShellWithoutRunningAnything(t *testing.T) {
	tests := []struct {
		line string
		want []string // nil: rx is not the command word
	}{
		{`rx "a b" 'c d' e\ f`, []string{"rx", "a b", "c d", "e f"}},
		{`$ RX_HIDDEN=true time rx x ~/  # comment`, []string{"rx", "x", "~/"}},
		{`rx "pattern" file.log --json > out.json`, []string{"rx", "pattern", "file.log", "--json"}},
		{`rx serve 2>>/var/log/rx.log`, []string{"rx", "serve"}},
		{`rx samples a.log -c 3 # three lines`, []string{"rx", "samples", "a.log", "-c", "3"}},
		{`rx index a.log --json | jq .`, []string{"rx", "index", "a.log", "--json"}},
		{`RX_API_TOKEN="$(openssl rand -hex 24)" rx serve`, []string{"rx", "serve"}},
		{`rx samples a.log --offsets="$offsets"`, []string{"rx", "samples", "a.log", "--offsets=$offsets"}},
		{`find /var/log -exec rx index {} \;`, nil},
		{`# rx version dev`, nil},
	}
	for _, tc := range tests {
		got := rxCommandWords(tc.line)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") || (got == nil) != (tc.want == nil) {
			t.Errorf("rxCommandWords(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// The pages the docs tests read are the published ones: README.md and
// the Markdown pages under docs/, without the gitignored working
// directories there (docs/plans, docs/superpowers), whose files exist
// on one machine only.
func TestDocPages_LeaveOutTheGitignoredWorkingDirectories(t *testing.T) {
	root := t.TempDir()
	for _, page := range []string{
		"README.md", "docs/index.md", "docs/cli/logs.md", "docs/notes.txt",
		"docs/plans/a-plan.md", "docs/superpowers/specs/a-spec.md", "docs/cli/plans/kept.md",
	} {
		path := filepath.Join(root, filepath.FromSlash(page))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# page\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, page := range docPagesUnder(t, root) {
		rel, err := filepath.Rel(root, page)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(rel))
	}
	slices.Sort(got)
	want := []string{"README.md", "docs/cli/logs.md", "docs/cli/plans/kept.md", "docs/index.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("pages %v, want %v", got, want)
	}
}
