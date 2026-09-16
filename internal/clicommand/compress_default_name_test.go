package clicommand

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
)

// defaultNameText is the text the fixtures below hold.
func defaultNameText() []byte {
	var b bytes.Buffer
	for n := 1; n <= 3000; n++ {
		fmt.Fprintf(&b, "LINE %d level=info took %d ms\n", n, n%97)
	}
	return b.Bytes()
}

// writeInput writes body to name in dir and returns its path.
func writeInput(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// seekableFileText decompresses a seekable zstd file whole.
func seekableFileText(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test output path
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	r, err := compression.NewReader(f, compression.FormatSeekableZstd)
	if err != nil {
		t.Fatalf("%s is not a seekable zstd file: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	text, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return text
}

// Without --output, the output of a compressed input is named after
// the input without its compression suffix, beside the input or in
// --output-dir, and holds the input's text.
func TestCompress_DefaultOutputDropsTheCompressionSuffix(t *testing.T) {
	text := defaultNameText()
	for _, tc := range []struct{ format, name string }{
		{compressedcopy.Gzip, "app.log.gz"},
		{compressedcopy.Bzip2, "app.log.bz2"},
		{compressedcopy.Xz, "app.log.xz"},
		{compressedcopy.Zstd, "app.log.zstd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := compressedcopy.Encode(t, tc.format, text)
			if body == nil {
				t.Skipf("no %s binary on PATH to write the fixture", tc.format)
			}
			dir := t.TempDir()
			t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))
			input := writeInput(t, dir, tc.name, body)
			outDir := filepath.Join(dir, "out")

			for _, run := range []struct {
				outputDir, want string
			}{
				{"", filepath.Join(dir, "app.log.zst")},
				{outDir, filepath.Join(outDir, "app.log.zst")},
			} {
				entry, err := compressOne(t, compressParams{
					paths: []string{input}, outputDir: run.outputDir, frameSize: "16K", level: 1, workers: 1,
				})
				if err != nil {
					t.Fatalf("runCompress: %v (%v)", err, entry["error"])
				}
				if entry["output"] != run.want {
					t.Errorf("output = %v, want %s", entry["output"], run.want)
				}
				if got := seekableFileText(t, run.want); !bytes.Equal(got, text) {
					t.Errorf("%s holds %d bytes, want the input's %d bytes of text", run.want, len(got), len(text))
				}
			}
		})
	}
}

// The default output name may already exist: without --force it is
// refused and left as it was; with --force it is replaced.
func TestCompress_DefaultOutputThatExistsNeedsForce(t *testing.T) {
	text := defaultNameText()
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))
	input := writeInput(t, dir, "app.log.gz", compressedcopy.Encode(t, compressedcopy.Gzip, text))
	existing := writeInput(t, dir, "app.log.zst", []byte("an older file\n"))

	entry, err := compressOne(t, compressParams{paths: []string{input}, frameSize: "16K", level: 1, workers: 1})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitGenericError {
		t.Fatalf("error: got %v, want exit code %d", err, ExitGenericError)
	}
	want := "output file already exists: " + existing + " (use --force to overwrite)"
	if entry["error"] != want {
		t.Errorf("error = %v, want %q", entry["error"], want)
	}
	if got, _ := os.ReadFile(existing); string(got) != "an older file\n" { //nolint:gosec // test path
		t.Errorf("the refused run changed %s", existing)
	}

	entry, err = compressOne(t, compressParams{paths: []string{input}, frameSize: "16K", level: 1, workers: 1, force: true})
	if err != nil {
		t.Fatalf("runCompress --force: %v (%v)", err, entry["error"])
	}
	if got := seekableFileText(t, existing); !bytes.Equal(got, text) {
		t.Errorf("--force left %d bytes of text in %s, want %d", len(got), existing, len(text))
	}
}

// A plain (not seekable) zstd input named app.log.zst would get its own
// name as the default output. It is refused with and without --force,
// and the input is left as it was.
func TestCompress_PlainZstdInputIsNeverItsOwnDefaultOutput(t *testing.T) {
	body := compressedcopy.Encode(t, compressedcopy.Zstd, defaultNameText())
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))
	input := writeInput(t, dir, "app.log.zst", body)

	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			entry, err := compressOne(t, compressParams{
				paths: []string{input}, frameSize: "16K", level: 1, workers: 1, force: force,
			})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitGenericError {
				t.Fatalf("error: got %v, want exit code %d", err, ExitGenericError)
			}
			if want := "the output path is the input file (use --output to name another file)"; entry["error"] != want {
				t.Errorf("error = %v, want %q", entry["error"], want)
			}
			if got, _ := os.ReadFile(input); !bytes.Equal(got, body) { //nolint:gosec // test path
				t.Error("the input was changed")
			}
		})
	}
}
