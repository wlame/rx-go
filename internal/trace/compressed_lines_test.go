package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEncodedFixture writes the same numbered log three ways: plain,
// gzipped and as a seekable zstd file. Every answer about a line must
// be the same whichever one is searched.
func writeEncodedFixture(t *testing.T, lines int) (plain, gzipped, seekable string) {
	t.Helper()
	dir := t.TempDir()

	var text strings.Builder
	for n := 1; n <= lines; n++ {
		fmt.Fprintf(&text, "line %d NEEDLE of the log with padding to give frames some bulk\n", n)
	}
	raw := []byte(text.String())

	plain = filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, raw, 0o600); err != nil {
		t.Fatalf("write plain fixture: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip fixture: %v", err)
	}
	gzipped = filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(gzipped, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip fixture: %v", err)
	}

	// Small frames so the fixture holds several of them.
	seekable = writeSeekableZstdFile(t, raw, 64*1024)
	return plain, gzipped, seekable
}

// TestEveryEncodingReportsTheSameLineNumbers is the regression test for
// a match's line number depending on how the file was stored.
//
// A search of a .gz reported the line as unknown even though ripgrep
// had seen the whole file, and a search of a seekable .zst reported the
// line's position inside its frame — line 24,014 of a real log came
// back as 407.
func TestEveryEncodingReportsTheSameLineNumbers(t *testing.T) {
	requireRipgrep(t)
	plain, gzipped, seekable := writeEncodedFixture(t, 4000)

	search := func(path string) []struct {
		offset   int64
		relative int
		absolute int
		text     string
	} {
		resp, err := New().RunWithOptions(
			context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
		)
		if err != nil {
			t.Fatalf("search %s: %v", path, err)
		}
		out := make([]struct {
			offset   int64
			relative int
			absolute int
			text     string
		}, 0, len(resp.Matches))
		for _, m := range resp.Matches {
			rel := 0
			if m.RelativeLineNumber != nil {
				rel = *m.RelativeLineNumber
			}
			out = append(out, struct {
				offset   int64
				relative int
				absolute int
				text     string
			}{m.Offset, rel, m.AbsoluteLineNumber, *m.LineText})
		}
		return out
	}

	fromPlain := search(plain)
	if len(fromPlain) != 4000 {
		t.Fatalf("plain file: %d matches, want 4000", len(fromPlain))
	}
	for _, encoded := range []struct {
		name string
		path string
	}{{"gzip", gzipped}, {"seekable zstd", seekable}} {
		got := search(encoded.path)
		if len(got) != len(fromPlain) {
			t.Fatalf("%s: %d matches, plain file has %d", encoded.name, len(got), len(fromPlain))
		}
		for i := range fromPlain {
			if got[i] != fromPlain[i] {
				t.Fatalf("%s match %d: %+v, plain file says %+v", encoded.name, i, got[i], fromPlain[i])
			}
		}
	}
}

// TestCompressedMatchesCarryTheLineTheirTextNames checks the numbers
// against the fixture's own labeling, so an encoding that is
// consistently wrong cannot pass.
func TestCompressedMatchesCarryTheLineTheirTextNames(t *testing.T) {
	requireRipgrep(t)
	_, gzipped, seekable := writeEncodedFixture(t, 4000)

	for _, path := range []string{gzipped, seekable} {
		resp, err := New().RunWithOptions(
			context.Background(), []string{path}, []string{"NEEDLE"}, Options{NoCache: true},
		)
		if err != nil {
			t.Fatalf("search %s: %v", path, err)
		}
		for _, m := range resp.Matches {
			want := lineNumberFromText(t, *m.LineText)
			if m.AbsoluteLineNumber != want {
				t.Fatalf("%s: offset %d reported as line %d, its text says %d",
					filepath.Base(path), m.Offset, m.AbsoluteLineNumber, want)
			}
		}
	}
}
