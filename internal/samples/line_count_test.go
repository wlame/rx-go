package samples

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The line count an index records is the number rx samples gives the
// last line, so a reader that adds line counts up (one file after
// another) numbers each line as samples does: a last line without a line
// break after it is a line of its own, counted by the index and named by
// --lines=-1, in every storage, read with the index or without it.
func TestIndexLineCountIsTheLineThatMinusOneNames(t *testing.T) {
	const lines = 300
	for _, finalNewline := range []bool{true, false} {
		var buf bytes.Buffer
		for n := 1; n <= lines; n++ {
			fmt.Fprintf(&buf, "LINE %d of the log", n)
			if n < lines || finalNewline {
				buf.WriteByte('\n')
			}
		}
		text := buf.Bytes()
		lastLine := fmt.Sprintf("LINE %d of the log", lines)
		dir := t.TempDir()
		files := compressedCopiesOf(t, text, dir)
		files["app.log"] = filepath.Join(dir, "app.log")
		if err := os.WriteFile(files["app.log"], text, 0o600); err != nil {
			t.Fatalf("write plain: %v", err)
		}

		for name, path := range files {
			t.Run(fmt.Sprintf("%s/final newline %v", name, finalNewline), func(t *testing.T) {
				idx, err := index.Build(path, index.BuildOptions{StepBytes: 1024})
				if err != nil {
					t.Fatalf("index.Build: %v", err)
				}
				if idx.LineCount == nil || *idx.LineCount != lines {
					t.Fatalf("line_count = %v; want %d", idx.LineCount, lines)
				}
				count := *idx.LineCount
				key := strconv.FormatInt(count, 10)
				loaders := map[string]IndexLoader{
					"no index": NoIndex,
					"indexed":  func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil },
				}
				for how, loader := range loaders {
					resp, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: -1}, {Start: count}, {Start: count + 1}}, IndexLoader: loader})
					if err != nil {
						t.Fatalf("%s: Resolve: %v", how, err)
					}
					if got := resp.Samples[key]; len(got) != 1 || got[0] != lastLine {
						t.Errorf("%s: -1 and %d give %q; want the last line %q (lines map %v)", how, count, got, lastLine, resp.Lines)
					}
					past := strconv.FormatInt(count+1, 10)
					if got := resp.Lines[past]; got != -1 {
						t.Errorf("%s: line %d, past the count, starts at %d; want -1", how, count+1, got)
					}
				}
			})
		}
	}
}

// An empty file has no lines: its index counts none, and -1 names no
// line, with the index or without it.
func TestIndexLineCountOfAnEmptyFileIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx, err := index.Build(path, index.BuildOptions{})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if idx.LineCount == nil || *idx.LineCount != 0 {
		t.Fatalf("line_count = %v; want 0", idx.LineCount)
	}
	for how, loader := range map[string]IndexLoader{
		"no index": NoIndex,
		"indexed":  func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil },
	} {
		resp, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: -1}}, IndexLoader: loader})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", how, err)
		}
		for key, window := range resp.Samples {
			if len(window) != 0 {
				t.Errorf("%s: -1 gives %q under key %s; want no line", how, window, key)
			}
		}
	}
}
