package samples

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeGzipFixture writes a gzip file whose every line names its own
// line number, and returns both paths so a test can compare what the
// resolver reads out of each.
func writeGzipFixture(t *testing.T, lines int) (plain, compressed string) {
	t.Helper()
	dir := t.TempDir()
	plain = filepath.Join(dir, "app.log")
	compressed = plain + ".gz"

	var raw bytes.Buffer
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&raw, "line %d of the log\n", i)
	}
	if err := os.WriteFile(plain, raw.Bytes(), 0o600); err != nil {
		t.Fatalf("write plain fixture: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip fixture: %v", err)
	}
	if err := os.WriteFile(compressed, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip fixture: %v", err)
	}
	return plain, compressed
}

// TestResolveReadsLinesOutOfACompressedFile is the regression test for
// `rx samples file.gz --lines=N` printing raw compressed bytes: the CLI
// went down the plain-file path while only the HTTP route decompressed.
func TestResolveReadsLinesOutOfACompressedFile(t *testing.T) {
	plain, compressed := writeGzipFixture(t, 500)

	for _, tc := range []struct {
		name   string
		lines  string
		before int
		after  int
	}{
		{name: "single line", lines: "250"},
		{name: "line with context", lines: "250", before: 2, after: 2},
		{name: "range", lines: "10-14"},
		{name: "several", lines: "1,250,500"},
		{name: "counted from the end", lines: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseCSV(tc.lines)
			if err != nil {
				t.Fatalf("ParseCSV: %v", err)
			}
			req := func(path string) Request {
				return Request{
					Path: path, Lines: spec,
					BeforeContext: tc.before, AfterContext: tc.after,
					IndexLoader: NoIndex,
				}
			}
			fromPlain, err := Resolve(req(plain))
			if err != nil {
				t.Fatalf("resolve plain: %v", err)
			}
			fromGzip, err := Resolve(req(compressed))
			if err != nil {
				t.Fatalf("resolve gzip: %v", err)
			}
			if !fromGzip.IsCompressed {
				t.Error("the response does not report the file as compressed")
			}
			if fromGzip.CompressionFormat == nil || *fromGzip.CompressionFormat != "gzip" {
				t.Errorf("compression_format = %v, want gzip", fromGzip.CompressionFormat)
			}
			if len(fromGzip.Samples) != len(fromPlain.Samples) {
				t.Fatalf("gzip returned %d windows, plain %d: %v vs %v",
					len(fromGzip.Samples), len(fromPlain.Samples), fromGzip.Samples, fromPlain.Samples)
			}
			for key, want := range fromPlain.Samples {
				got, ok := fromGzip.Samples[key]
				if !ok {
					t.Fatalf("gzip has no window %q; it has %v", key, fromGzip.Samples)
				}
				if len(got) != len(want) {
					t.Fatalf("window %q: gzip has %d lines, plain %d", key, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("window %q line %d: gzip %q, plain %q", key, i, got[i], want[i])
					}
				}
			}
		})
	}
}

// TestResolveNumbersTheLinesItReadsFromACompressedFile checks the text
// against the line numbers the fixture carries, so a window that is
// offset by one cannot pass.
func TestResolveNumbersTheLinesItReadsFromACompressedFile(t *testing.T) {
	_, compressed := writeGzipFixture(t, 100)
	spec, err := ParseCSV("42")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	resp, err := Resolve(Request{
		Path: compressed, Lines: spec, BeforeContext: 1, AfterContext: 1, IndexLoader: NoIndex,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"line 41 of the log", "line 42 of the log", "line 43 of the log"}
	got := resp.Samples["42"]
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestResolveRefusesByteOffsetsOnACompressedFile pins the one thing a
// compressed file cannot answer.
func TestResolveRefusesByteOffsetsOnACompressedFile(t *testing.T) {
	_, compressed := writeGzipFixture(t, 20)
	spec, err := ParseCSV("100")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	_, err = Resolve(Request{Path: compressed, Offsets: spec, IndexLoader: NoIndex})
	if !errors.Is(err, ErrOffsetsOnCompressed) {
		t.Fatalf("err = %v, want ErrOffsetsOnCompressed", err)
	}
}

// TestResolveCountsLinesFromTheEndOfACompressedFile covers the negative
// line number, which needs a first pass to learn where the end is.
func TestResolveCountsLinesFromTheEndOfACompressedFile(t *testing.T) {
	const lines = 300
	_, compressed := writeGzipFixture(t, lines)
	spec, err := ParseCSV("-1")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	resp, err := Resolve(Request{Path: compressed, Lines: spec, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	key := strconv.Itoa(lines)
	got := resp.Samples[key]
	if len(got) != 1 || got[0] != "line 300 of the log" {
		t.Fatalf("samples[%q] = %v", key, got)
	}
}
