package samples

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// A last line with no line break after it is still a line, so -1 names
// it in a compressed file as in the plain copy of the same text.
func TestResolveCountsAnUnterminatedLastLineOfACompressedFile(t *testing.T) {
	text, _ := variedLog(400, 9)
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(plainPath, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	request := func(path string, loader IndexLoader) Request {
		return Request{Path: path, Lines: []OffsetOrRange{{Start: -1}, {Start: -3}}, BeforeContext: 1, IndexLoader: loader}
	}
	plain, err := Resolve(request(plainPath, NoIndex))
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	if _, ok := plain.Lines["400"]; !ok {
		t.Fatalf("plain: -1 did not name line 400: %v", plain.Lines)
	}

	for name, path := range compressedCopiesOf(t, text, dir) {
		for how, loader := range loadersFor(t, path) {
			t.Run(name+"/"+how, func(t *testing.T) {
				got, err := Resolve(request(path, loader))
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if !reflect.DeepEqual(got.Lines, plain.Lines) || !reflect.DeepEqual(got.Samples, plain.Samples) {
					t.Errorf("got lines %v samples %q, want lines %v samples %q",
						got.Lines, got.Samples, plain.Lines, plain.Samples)
				}
			})
		}
	}
}

// TestResolveReportsTheByteOffsetOfACompressedLine pins the offset a
// compressed file reports for a line.
//
// It is a position in the decompressed stream, which is the coordinate
// system a search reports its matches in, so `samples` and `trace` name
// the same byte for the same line. Reporting -1 left the two surfaces
// unable to talk about the same place in the file.
func TestResolveReportsTheByteOffsetOfACompressedLine(t *testing.T) {
	plain, compressed := writeGzipFixture(t, 500)
	spec, err := ParseCSV("250")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}

	fromPlain, err := Resolve(Request{Path: plain, Lines: spec, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	fromGzip, err := Resolve(Request{Path: compressed, Lines: spec, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("resolve gzip: %v", err)
	}

	if fromGzip.Lines["250"] != fromPlain.Lines["250"] {
		t.Fatalf("gzip reports line 250 at byte %d, the plain file at %d",
			fromGzip.Lines["250"], fromPlain.Lines["250"])
	}
	if fromGzip.Lines["250"] <= 0 {
		t.Fatalf("no byte offset reported: %v", fromGzip.Lines)
	}
	// And the offset really is where that line starts.
	raw, err := os.ReadFile(plain) //nolint:gosec // fixture path from t.TempDir
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := 0
	for i := 1; i < 250; i++ {
		want += bytes.IndexByte(raw[want:], '\n') + 1
	}
	if int(fromGzip.Lines["250"]) != want {
		t.Fatalf("offset %d, counting the text gives %d", fromGzip.Lines["250"], want)
	}
}
