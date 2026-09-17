package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// The offsets a trace reports for a compressed file are positions in
// its decompressed text, so a match the scan left unnumbered is
// numbered from that text: through the file's index by default, and by
// counting from the start under --no-index.

// compressedFixtures writes a gzip copy and a seekable-zstd copy of a
// chunked plain fixture and returns the plain path, the two copies and
// the offset of every line.
func compressedFixtures(t *testing.T) (plain string, copies map[string]string, offsets map[int]int64) {
	t.Helper()
	plain, _ = writeChunkedFixture(t, "resolve.log", 256<<10)
	offsets = fixtureLineOffsets(t, plain)
	text, err := os.ReadFile(plain)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(text)
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(plain+".gz", gz.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}

	var zst bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 16 << 10, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &zst); err != nil {
		t.Fatalf("encode seekable: %v", err)
	}
	if err := os.WriteFile(plain+".zst", zst.Bytes(), 0o600); err != nil {
		t.Fatalf("write seekable: %v", err)
	}
	return plain, map[string]string{"gzip": plain + ".gz", "seekable zstd": plain + ".zst"}, offsets
}

func TestLineResolver_CompressedFileWithAnIndexIsNumbered(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	_, copies, offsets := compressedFixtures(t)
	lines := []int{1, 437, 900, len(offsets)}

	for name, path := range copies {
		t.Run(name, func(t *testing.T) {
			idx, err := index.Build(path, index.BuildOptions{StepBytes: 4096})
			if err != nil {
				t.Fatalf("build index: %v", err)
			}
			if _, err := index.Save(idx); err != nil {
				t.Fatalf("save index: %v", err)
			}
			matches := unnumberedMatches(offsets, lines)
			resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil, lineResolverFor(Options{}))
			for i, m := range matches {
				if m.AbsoluteLineNumber != lines[i] {
					t.Errorf("offset %d: absolute_line_number = %d, want %d", m.Offset, m.AbsoluteLineNumber, lines[i])
				}
				if m.RelativeLineNumber == nil || *m.RelativeLineNumber != lines[i] {
					t.Errorf("offset %d: relative_line_number = %v, want %d", m.Offset, m.RelativeLineNumber, lines[i])
				}
			}
		})
	}
}

func TestLineResolver_CompressedFileWithoutAnIndexStaysUnknown(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	_, copies, offsets := compressedFixtures(t)

	for name, path := range copies {
		t.Run(name, func(t *testing.T) {
			matches := unnumberedMatches(offsets, []int{900})
			resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil, lineResolverFor(Options{}))
			if got := matches[0].AbsoluteLineNumber; got != -1 {
				t.Errorf("absolute_line_number = %d, want -1 without an index", got)
			}
		})
	}
}

func TestLineResolver_NoIndexCountsTheLinesOfACompressedFile(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cacheDir)
	_, copies, offsets := compressedFixtures(t)
	lines := []int{2, 437, len(offsets)}
	before := cacheFiles(t, cacheDir)

	for name, path := range copies {
		t.Run(name, func(t *testing.T) {
			matches := unnumberedMatches(offsets, lines)
			resolveUnknownLineNumbers(map[string]sandbox.Pinned{"f1": pinForTest(t, path)}, matches, nil,
				lineResolverFor(Options{NoIndex: true}))
			for i, m := range matches {
				if m.AbsoluteLineNumber != lines[i] {
					t.Errorf("offset %d: absolute_line_number = %d, want %d", m.Offset, m.AbsoluteLineNumber, lines[i])
				}
			}
		})
	}
	if after := cacheFiles(t, cacheDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("cache directory changed: before %v, after %v", before, after)
	}
}
