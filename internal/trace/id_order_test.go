package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// writeFilesInOrder writes n files app1.log … appN.log, file k holding
// content(k), and returns their paths in that order. Passing the paths
// as a list, not their directory, makes app<k>.log the file with id
// f<k>: ids follow the order the paths are given in.
func writeFilesInOrder(t *testing.T, n int, content func(k int) string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, n)
	for k := 1; k <= n; k++ {
		p := filepath.Join(dir, fmt.Sprintf("app%d.log", k))
		if err := os.WriteFile(p, []byte(content(k)), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// matchOrder lists each match as "<file id>/<pattern id>", in the
// answer's order.
func matchOrder(matches []rxtypes.Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.File+"/"+m.Pattern)
	}
	return out
}

// Twelve files, one match each: the matches come in the order the files
// were given, f2 before f10.
func TestEngine_Run_ListsMatchesInFileOrderPastNineFiles(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	paths := writeFilesInOrder(t, 12, func(k int) string {
		return fmt.Sprintf("LINE 1 file=%d error\n", k)
	})

	resp, err := New().RunWithOptions(context.Background(), paths, []string{"error"}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(resp.Matches) != 12 {
		t.Fatalf("got %d matches, want 12: %v", len(resp.Matches), matchOrder(resp.Matches))
	}
	for i, m := range resp.Matches {
		k := i + 1
		if m.File != fmt.Sprintf("f%d", k) || resp.Files[m.File] != paths[i] {
			t.Errorf("match %d: file %s (%s), want f%d (%s)", i, m.File, resp.Files[m.File], k, paths[i])
		}
		if m.LineText == nil || !strings.Contains(*m.LineText, fmt.Sprintf("file=%d ", k)) {
			t.Errorf("match %d: line %q, want the line of app%d.log", i, derefText(m.LineText), k)
		}
	}
}

// The cap cuts the matches after they are put in order, so it keeps the
// first files' matches. Each file's one line matches both patterns,
// which makes it two matches for one matched line: the engine reads
// files until the matched lines reach the cap, so it reads f1 to f10
// and finds 20 matches, and the cut to 10 keeps those of f1 to f5.
func TestEngine_Run_CapKeepsTheMatchesOfTheFirstFiles(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	paths := writeFilesInOrder(t, 12, func(k int) string {
		return fmt.Sprintf("LINE 1 file=%d alpha beta\n", k)
	})
	limit := 10

	resp, err := New().RunWithOptions(context.Background(), paths, []string{"alpha", "beta"},
		Options{MaxResults: &limit})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"f1/p1", "f1/p2", "f2/p1", "f2/p2", "f3/p1",
		"f3/p2", "f4/p1", "f4/p2", "f5/p1", "f5/p2",
	}
	if got := matchOrder(resp.Matches); !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// elevenWords is one line that each of the eleven patterns below
// matches once.
const elevenWords = "ant bee cat dog eel fox gnu hen ibis jay kiwi"

// elevenPatterns are the words of elevenWords, so p<k> is word k.
var elevenPatterns = strings.Fields(elevenWords)

// Eleven patterns on one line: one match each, at one offset, in
// pattern order, p2 before p10.
func TestEngine_Run_OrdersPatternsByNumberOnOneLine(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := mustWriteFile(t, []byte(elevenWords+"\n"))

	resp, err := New().RunWithOptions(context.Background(), []string{p}, elevenPatterns, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := make([]string, 0, len(elevenPatterns))
	for k := 1; k <= len(elevenPatterns); k++ {
		want = append(want, fmt.Sprintf("f1/p%d", k))
	}
	if got := matchOrder(resp.Matches); !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// Under a cap the patterns kept are the first ones given.
func TestEngine_Run_CapKeepsTheFirstPatternsOnOneLine(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	p := mustWriteFile(t, []byte(elevenWords+"\n"))
	limit := 5

	resp, err := New().RunWithOptions(context.Background(), []string{p}, elevenPatterns,
		Options{MaxResults: &limit})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"f1/p1", "f1/p2", "f1/p3", "f1/p4", "f1/p5"}
	if got := matchOrder(resp.Matches); !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// The context windows are those of the matches the cap keeps, so they
// follow the same order: the windows of f1 to f5, none of f10.
func TestEngine_Run_CapKeepsTheContextWindowsOfTheFirstFiles(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	firstLine := func(k int) string { return fmt.Sprintf("LINE 1 file=%d\n", k) }
	paths := writeFilesInOrder(t, 12, func(k int) string {
		return firstLine(k) + fmt.Sprintf("LINE 2 file=%d alpha beta\nLINE 3 file=%d\n", k, k)
	})
	limit := 10

	resp, err := New().RunWithOptions(context.Background(), paths, []string{"alpha", "beta"},
		Options{MaxResults: &limit, ContextBefore: 1, ContextAfter: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var want []string
	for k := 1; k <= 5; k++ {
		for p := 1; p <= 2; p++ {
			want = append(want, fmt.Sprintf("p%d:f%d:%d", p, k, len(firstLine(k))))
		}
	}
	got := make([]string, 0, len(resp.ContextLines))
	for key := range resp.ContextLines {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("context_lines keys %v, want %v", got, want)
	}
}
