package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// scanTimeLimit bounds every scan in this file. The scans take a few
// seconds; a scan that reaches the limit is one that would otherwise
// block for ever, and the test fails instead of stalling the suite.
const scanTimeLimit = 90 * time.Second

// finishWithin runs scan with a context that expires after
// scanTimeLimit (or shortly before the test binary's own deadline,
// whichever comes first) and fails the test when scan reaches that
// limit.
//
// Two guards, because a hang can take two shapes. When the blocked code
// watches the context (rg is started with exec.CommandContext), the
// expiry kills rg and scan returns: the ctx.Err check below then fails
// the test. When nothing watches it, scan never returns: the select
// gives up a little after the limit and fails the test, leaving the
// stuck goroutine behind (the test binary exits soon after anyway).
//
// scan runs on another goroutine, so it must not call t.Fatal; it
// stores its results in variables the caller reads after finishWithin.
func finishWithin(t *testing.T, scan func(ctx context.Context)) {
	t.Helper()
	limit := scanTimeLimit
	if deadline, ok := t.Deadline(); ok {
		if untilDeadline := time.Until(deadline) - 10*time.Second; untilDeadline < limit {
			limit = untilDeadline
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	// done is closed when scan returns. Receiving from a closed channel
	// never blocks, so the select below wakes as soon as that happens.
	done := make(chan struct{})
	go func() {
		defer close(done)
		scan(ctx)
	}()
	select {
	case <-done:
	case <-time.After(limit + 5*time.Second):
		t.Fatalf("the scan did not return within %s", limit)
	}
	if ctx.Err() != nil {
		t.Fatalf("the scan ran until its %s deadline killed it: without one it would never return", limit)
	}
}

// largeEventCase is a log whose first line makes one ripgrep JSON event
// larger than 16 MiB, followed by ordinary matching lines.
type largeEventCase struct {
	name    string
	pattern string
	// text is the whole log: lines numbered 1..lineCount, every one of
	// which matches pattern.
	text      []byte
	lineCount int
	// firstLineSubmatches is how many times pattern matches line 1.
	firstLineSubmatches int
}

// largeEventCases builds the two ways one event outgrows 16 MiB:
//
//   - a line on which the pattern matches every character. Each
//     submatch adds a JSON object of about 50 bytes to the line's
//     event, so 350,000 of them make an event of about 18 MB from a
//     line of 350 KB.
//   - a matched line of 17 MB, whose text alone is larger than 16 MiB.
//
// In both, matching lines follow the large one, so ripgrep still has
// output to write after it.
func largeEventCases() []largeEventCase {
	const lineCount = 3000
	build := func(firstLine, laterLine string) []byte {
		var b strings.Builder
		b.WriteString(firstLine)
		for i := 2; i <= lineCount; i++ {
			b.WriteString("LINE " + strconv.Itoa(i) + " " + laterLine + "\n")
		}
		return []byte(b.String())
	}
	const everyCharacter = 350_000
	const longLine = 17_000_000
	return []largeEventCase{
		{
			name:                "pattern matches every character of a line",
			pattern:             "x",
			text:                build("LINE 1 "+strings.Repeat("x", everyCharacter)+"\n", "x short"),
			lineCount:           lineCount,
			firstLineSubmatches: everyCharacter,
		},
		{
			name:                "matched line longer than 16 MiB",
			pattern:             "NEEDLE",
			text:                build("LINE 1 NEEDLE "+strings.Repeat("x", longLine)+"\n", "NEEDLE short"),
			lineCount:           lineCount,
			firstLineSubmatches: 1,
		},
	}
}

// storedForm writes text the way one storage form keeps it and returns
// the path. Each form takes a different path through the engine: plain
// chunks, a decompressing stream, or seekable frames.
type storedForm struct {
	name  string
	write func(t *testing.T, text []byte) string
}

func storedForms() []storedForm {
	writeEncoded := func(format, fileName string) func(*testing.T, []byte) string {
		return func(t *testing.T, text []byte) string {
			p := filepath.Join(t.TempDir(), fileName)
			if err := os.WriteFile(p, compressedcopy.Encode(t, format, text), 0o600); err != nil {
				t.Fatalf("write %s: %v", p, err)
			}
			return p
		}
	}
	return []storedForm{
		{name: "plain", write: func(t *testing.T, text []byte) string { return mustWriteFile(t, text) }},
		{name: "gzip", write: writeEncoded(compressedcopy.Gzip, "log.gz")},
		{name: "plain zstd", write: writeEncoded(compressedcopy.Zstd, "log.zst")},
		// 1 MiB frames, so the long line spans many of them.
		{name: "seekable zstd", write: func(t *testing.T, text []byte) string {
			return writeSeekableZstdFile(t, text, 1024*1024)
		}},
	}
}

// A ripgrep event has no size limit, so neither may the parser: a line
// whose event is larger than 16 MiB is answered like any other, along
// with every match after it, whichever way the file is stored.
func TestTraceAnswersALineWhoseRipgrepEventExceeds16MiB(t *testing.T) {
	requireRipgrep(t)
	for _, tc := range largeEventCases() {
		for _, form := range storedForms() {
			t.Run(tc.name+"/"+form.name, func(t *testing.T) {
				path := form.write(t, tc.text)

				var resp *rxtypes.TraceResponse
				var err error
				finishWithin(t, func(ctx context.Context) {
					resp, err = New().RunWithOptions(ctx, []string{path}, []string{tc.pattern}, Options{NoCache: true})
				})

				if err != nil {
					t.Fatalf("RunWithOptions: %v", err)
				}
				if len(resp.SkippedFiles) != 0 {
					t.Fatalf("skipped_files = %v, want none", resp.SkippedFiles)
				}
				if len(resp.Matches) != tc.lineCount {
					t.Fatalf("got %d matches, want %d", len(resp.Matches), tc.lineCount)
				}
				first, last := resp.Matches[0], resp.Matches[len(resp.Matches)-1]
				if first.AbsoluteLineNumber != 1 || len(first.Submatches) != tc.firstLineSubmatches {
					t.Errorf("first match: line %d with %d submatches, want line 1 with %d",
						first.AbsoluteLineNumber, len(first.Submatches), tc.firstLineSubmatches)
				}
				if last.AbsoluteLineNumber != tc.lineCount {
					t.Errorf("last match on line %d, want %d", last.AbsoluteLineNumber, tc.lineCount)
				}
			})
		}
	}
}

// installRipgrepThatWritesGarbage puts a stand-in for rg first on PATH.
// It ignores its arguments and input, writes one line that is not JSON,
// and then far more valid events than a pipe buffer holds, so it blocks
// on its stdout unless the reader keeps reading or kills it.
//
// The script execs awk, so the process the scan starts is the one that
// writes: killing it leaves no child holding the pipes open.
func installRipgrepThatWritesGarbage(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		`exec awk 'BEGIN { print "this is not json"; for (i = 0; i < 200000; i++) print "{\"type\":\"begin\",\"data\":{}}" }'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake rg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Output the parser rejects ends the scan with an error, and rg, which
// still has output to write, is stopped rather than waited on for ever.
func TestOutputTheParserRejectsEndsTheScanWithAnError(t *testing.T) {
	text := []byte("LINE 1 NEEDLE\nLINE 2 NEEDLE\n")
	patternIDs, order := map[string]string{"p1": "NEEDLE"}, []string{"p1"}
	// write runs on the test goroutine and may fail the test; scan runs
	// inside finishWithin and only returns its error.
	scans := []struct {
		name  string
		write func(t *testing.T) string
		scan  func(ctx context.Context, path string) error
	}{
		{
			name:  "plain chunk",
			write: func(t *testing.T) string { return mustWriteFile(t, text) },
			scan: func(ctx context.Context, path string) error {
				task := FileTask{FilePath: path, Count: int64(len(text))}
				_, err := ProcessChunk(ctx, ChunkRequest{Task: task, PatternIDs: patternIDs, PatternOrder: order})
				return err
			},
		},
		{
			name:  "gzip stream",
			write: func(t *testing.T) string { return writeGzipFile(t, text) },
			scan: func(ctx context.Context, path string) error {
				_, _, _, err := ProcessCompressed(ctx, path, compression.FormatGzip, patternIDs, order, nil, 0, 0, nil)
				return err
			},
		},
		{
			name:  "seekable frames",
			write: func(t *testing.T) string { return writeSeekableZstdFile(t, text, 1024) },
			scan: func(ctx context.Context, path string) error {
				_, _, _, err := ProcessSeekable(ctx, path, patternIDs, order, nil, 0, 0, nil)
				return err
			},
		},
	}
	for _, sc := range scans {
		t.Run(sc.name, func(t *testing.T) {
			path := sc.write(t)
			installRipgrepThatWritesGarbage(t)

			var err error
			finishWithin(t, func(ctx context.Context) { err = sc.scan(ctx, path) })

			// rg was killed because of the parse error; the error the
			// scan reports is that cause, not rg's "killed" exit.
			var syntaxErr *json.SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("the scan reported %v, want the JSON parse error", err)
			}
		})
	}
}
