package trace

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// cappedRuleFixture writes a plain file of four chunks whose needles
// all lie in the last chunk, every tenth line from its sixth. A cap
// below the needle count keeps that chunk's first matches on every run,
// so two capped answers hold the same matches. (Where needles lie in
// several chunks, a cap keeps whichever matches the workers found
// first, and two runs can hold different ones.)
func cappedRuleFixture(t *testing.T) (path string, text []byte) {
	t.Helper()
	chunkedTraceEnv(t)
	t.Setenv("RX_LARGE_FILE_MB", "1")
	text = writeNeedleText(8<<20, func(int) bool { return false })
	path = writeTextFile(t, "capped.log", text)
	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("%d chunk(s); the test needs at least 2", len(tasks))
	}
	table := newTextLineTable(text)
	first := table.byStart[tasks[len(tasks)-1].Offset] + 1 // 1-based number of the last chunk's first line
	for n := first + 5; n < first+2000; n += 10 {
		markNeedles(text, table, n)
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	return path, text
}

// A capped trace answers alike with the cache empty, with a line index,
// from the trace cache and with --no-index: every field is equal, except
// that a line the cold scan left at -1 may be numbered by the others.
// Each of the other three numbers every line, and those three are equal
// in every field. Whether the cold scan leaves a line at -1 depends on
// how far the chunks before the last one got when the cap canceled
// them, so the test does not demand one; the test after it covers that
// case on a real answer.
func TestCappedTraceAgreesColdIndexedCachedAndWithoutAnIndex(t *testing.T) {
	path, text := cappedRuleFixture(t)
	limit := 40
	capped := Options{MaxResults: &limit, ContextBefore: 2, ContextAfter: 2}
	uncached := capped
	uncached.NoCache = true

	cold := traceOnce(t, path, []string{"NEEDLE"}, uncached)
	withoutIndex := uncached
	withoutIndex.NoIndex = true
	noIndex := traceOnce(t, path, []string{"NEEDLE"}, withoutIndex)

	idx, err := index.Build(path, index.BuildOptions{StepBytes: 64 << 10})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save index: %v", err)
	}
	indexed := traceOnce(t, path, []string{"NEEDLE"}, uncached)

	traceOnce(t, path, []string{"NEEDLE"}, Options{ContextBefore: 2, ContextAfter: 2})
	requireTraceCache(t, path)
	cached := cappedTraceFromCache(t, path, []string{"NEEDLE"}, capped)

	if len(cold.Matches) != limit {
		t.Fatalf("the cold trace returned %d matches, want %d", len(cold.Matches), limit)
	}
	for name, got := range map[string]*rxtypes.TraceResponse{"line index": indexed, "cache hit": cached, "--no-index": noIndex} {
		traceanswer.RequireAgree(t, name, got, cold, text)
		traceanswer.RequireSame(t, name+" against --no-index", got, noIndex)
	}
}

// The rule catches a line number that is resolved wrong. Starting from
// a real capped answer that numbers every line, the same answer with a
// match or a context line set back to -1 agrees with it, and the same
// answer with that number one line off disagrees with the -1 answer,
// whichever side of the comparison either stands on.
func TestAWrongResolvedLineNumberBreaksTheRule(t *testing.T) {
	path, text := cappedRuleFixture(t)
	limit := 40
	numbered := traceOnce(t, path, []string{"NEEDLE"}, Options{
		MaxResults: &limit, ContextBefore: 2, ContextAfter: 2, NoCache: true, NoIndex: true,
	})
	if n := traceanswer.UnnumberedLines(numbered); n != 0 {
		t.Fatalf("--no-index left %d line numbers at -1", n)
	}
	match := numbered.Matches[limit/2]
	if len(numbered.ContextLines[contextKey(match)]) == 0 {
		t.Fatalf("no context window for the match at byte %d", match.Offset)
	}

	for name, pick := range map[string]func(map[string]any) map[string]any{
		"match":        pickMatch(limit / 2),
		"context line": pickContext(contextKey(match)),
	} {
		t.Run(name, func(t *testing.T) {
			unresolved := withLine(t, numbered, pick, unnumber)
			wrong := withLine(t, numbered, pick, numberOneLineDown)

			if diff := traceanswer.Difference(numbered, unresolved, text); diff != "" {
				t.Errorf("the true number against -1 should agree, got %q", diff)
			}
			for order, pair := range map[string][2]map[string]any{
				"wrong against -1": {wrong, unresolved},
				"-1 against wrong": {unresolved, wrong},
			} {
				if diff := traceanswer.Difference(pair[0], pair[1], text); !strings.Contains(diff, "which is on line") {
					t.Errorf("%s: difference = %q, want one naming the line the byte is on", order, diff)
				}
			}
		})
	}
}

// withLine returns resp as a parsed JSON document in which set has
// changed the line object pick selects.
func withLine(t *testing.T, resp *rxtypes.TraceResponse, pick, set func(map[string]any) map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	set(pick(doc))
	return doc
}

// unnumber leaves a line the way a capped scan does: -1, with a
// chunk-relative relative number.
func unnumber(line map[string]any) map[string]any {
	line["absolute_line_number"], line["relative_line_number"] = float64(-1), float64(3)
	return line
}

// numberOneLineDown numbers a line as the line after it.
func numberOneLineDown(line map[string]any) map[string]any {
	n := line["absolute_line_number"].(float64) + 1
	line["absolute_line_number"], line["relative_line_number"] = n, n
	return line
}

// contextKey is the context_lines key of a match's window.
func contextKey(m rxtypes.Match) string {
	return m.Pattern + ":" + m.File + ":" + strconv.FormatInt(m.Offset, 10)
}

// pickMatch selects the i-th match of a parsed answer.
func pickMatch(i int) func(map[string]any) map[string]any {
	return func(doc map[string]any) map[string]any {
		return doc["matches"].([]any)[i].(map[string]any)
	}
}

// pickContext selects the first line of the window under key.
func pickContext(key string) func(map[string]any) map[string]any {
	return func(doc map[string]any) map[string]any {
		return doc["context_lines"].(map[string]any)[key].([]any)[0].(map[string]any)
	}
}
