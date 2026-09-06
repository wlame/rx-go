package trace

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeChunkedFixture writes a file whose every line carries its own
// 1-based line number, sized so the chunker splits it into more than
// one chunk. It returns the path and the number of lines.
//
// The caller must have set RX_MIN_CHUNK_SIZE_MB small enough for the
// size passed here to produce several chunks.
func writeChunkedFixture(t *testing.T, name string, targetBytes int) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriterSize(f, 1<<20)
	written, line := 0, 0
	// Padding makes lines long enough that the fixture reaches the
	// target size without a million-line loop.
	pad := strings.Repeat("x", 200)
	for written < targetBytes {
		line++
		// Every tenth line is the needle, so matches land in every chunk.
		kind := "filler"
		if line%10 == 0 {
			kind = "NEEDLE"
		}
		n, err := fmt.Fprintf(w, "line %d %s %s\n", line, kind, pad)
		if err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		written += n
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush fixture: %v", err)
	}
	return path, line
}

// lineNumberFromText pulls the line number a fixture line carries in
// its own text, which is the independent expectation every assertion
// here compares against.
func lineNumberFromText(t *testing.T, text string) int {
	t.Helper()
	fields := strings.Fields(text)
	if len(fields) < 2 || fields[0] != "line" {
		t.Fatalf("fixture line has no number: %q", text)
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("fixture line number %q: %v", fields[1], err)
	}
	return n
}

// TestTraceChunkedFileReportsAbsoluteLineNumbers is the regression test
// for line numbers that counted from the start of a chunk instead of
// the start of the file. Every match must report the line number its
// own text names, in both line-number fields.
func TestTraceChunkedFileReportsAbsoluteLineNumbers(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path, _ := writeChunkedFixture(t, "chunked.log", 4<<20)

	tasks, err := CreateFileTasks(path)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("fixture produced %d chunk(s); the test needs at least 2", len(tasks))
	}

	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.Matches) == 0 {
		t.Fatal("no matches")
	}

	for _, m := range resp.Matches {
		want := lineNumberFromText(t, *m.LineText)
		if m.AbsoluteLineNumber != want {
			t.Fatalf("offset %d: absolute_line_number = %d, want %d (line text %q)",
				m.Offset, m.AbsoluteLineNumber, want, *m.LineText)
		}
		if m.RelativeLineNumber == nil || *m.RelativeLineNumber != want {
			t.Fatalf("offset %d: relative_line_number = %v, want %d",
				m.Offset, m.RelativeLineNumber, want)
		}
	}
}

// TestTraceChunkedFileNumbersContextLines covers the same rule for the
// context lines around a match, which are numbered by the same running
// count.
func TestTraceChunkedFileNumbersContextLines(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path, _ := writeChunkedFixture(t, "chunked-ctx.log", 4<<20)

	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{NoCache: true, ContextBefore: 1, ContextAfter: 1},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.ContextLines) == 0 {
		t.Fatal("no context lines")
	}
	checked := 0
	for key, lines := range resp.ContextLines {
		for _, cl := range lines {
			want := lineNumberFromText(t, cl.LineText)
			if cl.AbsoluteLineNumber != want || cl.RelativeLineNumber != want {
				t.Fatalf("%s: context line numbered %d/%d, want %d (text %q)",
					key, cl.RelativeLineNumber, cl.AbsoluteLineNumber, want, cl.LineText)
			}
			checked++
		}
	}
	t.Logf("checked %d context lines", checked)
}

// TestTraceLineNumbersSurviveALineLongerThanTheLookahead pins the
// degenerate case: a line longer than the chunker's 256 KB newline
// lookahead splits a chunk mid-line. The running newline count still
// numbers the lines around it correctly because the split line
// contributes its newline to whichever chunk holds it.
func TestTraceLineNumbersSurviveALineLongerThanTheLookahead(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")

	path := filepath.Join(t.TempDir(), "long-line.log")
	var b strings.Builder
	line := 0
	writeLine := func(text string) {
		line++
		fmt.Fprintf(&b, "line %d %s\n", line, text)
	}
	for b.Len() < 1<<20 {
		writeLine("filler NEEDLE")
	}
	// One line well past the 256 KB lookahead.
	writeLine(strings.Repeat("y", 600*1024) + " NEEDLE")
	for b.Len() < 3<<20 {
		writeLine("filler NEEDLE")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}

	mismatches := 0
	for _, m := range resp.Matches {
		if m.AbsoluteLineNumber != lineNumberFromText(t, *m.LineText) {
			mismatches++
		}
	}
	if mismatches != 0 {
		t.Fatalf("%d of %d matches carry the wrong line number", mismatches, len(resp.Matches))
	}
}

// TestTraceSingleChunkFileKeepsAbsoluteLineNumbers guards the small-file
// path, where every chunk boundary rule collapses to "one chunk".
func TestTraceSingleChunkFileKeepsAbsoluteLineNumbers(t *testing.T) {
	requireRipgrep(t)
	path := filepath.Join(t.TempDir(), "small.log")
	content := "line 1 NEEDLE\nline 2 filler\nline 3 NEEDLE\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	want := []int{1, 3}
	if len(resp.Matches) != len(want) {
		t.Fatalf("got %d matches, want %d", len(resp.Matches), len(want))
	}
	for i, m := range resp.Matches {
		if m.AbsoluteLineNumber != want[i] {
			t.Errorf("match %d: absolute_line_number = %d, want %d", i, m.AbsoluteLineNumber, want[i])
		}
	}
}
