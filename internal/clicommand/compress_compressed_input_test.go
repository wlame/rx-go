package clicommand

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// compressedInputFixture writes a log whose lines read "LINE <n> ..."
// and its gzip copy, and returns both paths and the text.
func compressedInputFixture(t *testing.T) (dir, plain, gz string, text []byte) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))

	var body bytes.Buffer
	for n := 1; n <= 20000; n++ {
		fmt.Fprintf(&body, "LINE %d status=%d took %d ms\n", n, 200+n%5, n%311)
	}
	text = body.Bytes()
	plain = filepath.Join(dir, "app.log")
	if err := os.WriteFile(plain, text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}

	var gzBody bytes.Buffer
	w := gzip.NewWriter(&gzBody)
	_, _ = w.Write(text)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	gz = filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(gz, gzBody.Bytes(), 0o600); err != nil {
		t.Fatalf("write gz: %v", err)
	}
	return dir, plain, gz, text
}

// compressOne runs rx compress --json over one path and returns its
// entry together with the command's error.
func compressOne(t *testing.T, p compressParams) (map[string]any, error) {
	t.Helper()
	var buf bytes.Buffer
	p.jsonOutput = true
	runErr := runCompress(&buf, p)
	var decoded compressResult
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, buf.String())
	}
	if len(decoded.Files) != 1 {
		t.Fatalf("files: got %d entries, want 1", len(decoded.Files))
	}
	return decoded.Files[0], runErr
}

// traceMatches runs the trace engine over one file, with no cache, and
// returns its matches.
func traceMatches(t *testing.T, path, pattern string) []rxtypes.Match {
	t.Helper()
	resp, err := trace.New().RunWithOptions(context.Background(), []string{path}, []string{pattern},
		trace.Options{NoCache: true, NoIndex: true})
	if err != nil {
		t.Fatalf("trace %s: %v", path, err)
	}
	return resp.Matches
}

func TestCompress_GzipInputTracesLikeThePlainLog(t *testing.T) {
	dir, plain, gz, text := compressedInputFixture(t)
	output := filepath.Join(dir, "from-gz.zst")

	entry, err := compressOne(t, compressParams{
		paths: []string{gz}, output: output,
		frameSize: "64K", level: 3, workers: 2, buildIdx: false,
	})
	if err != nil {
		t.Fatalf("runCompress: %v (%v)", err, entry["error"])
	}
	if !seekable.IsSeekable(output) {
		t.Fatal("the output is not a seekable zstd file")
	}
	if size, _ := entry["decompressed_size"].(float64); int64(size) != int64(len(text)) {
		t.Errorf("decompressed_size: got %v, want the text's %d bytes", entry["decompressed_size"], len(text))
	}

	const pattern = `LINE 1[0-9]*7 status=202`
	want := traceMatches(t, plain, pattern)
	got := traceMatches(t, output, pattern)
	if len(want) == 0 {
		t.Fatal("the pattern matched nothing in the plain log")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trace of the output differs from the plain log: %d vs %d matches", len(got), len(want))
		for i := 0; i < len(got) && i < len(want) && i < 3; i++ {
			t.Logf("got %+v\nwant %+v", got[i], want[i])
		}
	}
}

func TestCompress_RefusesWhatItCannotCompress(t *testing.T) {
	dir, plain, _, _ := compressedInputFixture(t)

	seekableInput := filepath.Join(dir, "already.zst")
	if _, err := compressOne(t, compressParams{
		paths: []string{plain}, output: seekableInput, frameSize: "64K", level: 3, workers: 1,
	}); err != nil {
		t.Fatalf("write the seekable fixture: %v", err)
	}
	archive := filepath.Join(dir, "logs.tar.gz")
	if err := os.WriteFile(archive, []byte("\x1f\x8b not really"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	cases := []struct {
		name        string
		input       string
		output      string
		wantMessage string
	}{
		{"seekable zstd input", seekableInput, filepath.Join(dir, "again.zst"),
			"already a seekable zstd file (use --force to re-encode it)"},
		{"compound archive", archive, "", "compound archives (tar.gz, etc.) are not supported"},
		{"output is the input", plain, plain, "the output path is the input file (use --output to name another file)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := compressOne(t, compressParams{
				paths: []string{tc.input}, output: tc.output, frameSize: "64K", level: 3, workers: 1,
				force: tc.name == "output is the input",
			})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != ExitGenericError {
				t.Errorf("error: got %v, want exit code %d", err, ExitGenericError)
			}
			if msg, _ := entry["error"].(string); msg != tc.wantMessage {
				t.Errorf("error: got %q, want %q", msg, tc.wantMessage)
			}
			if tc.output != "" && tc.output != tc.input {
				if _, statErr := os.Stat(tc.output); statErr == nil {
					t.Errorf("a refused input still wrote %s", tc.output)
				}
			}
		})
	}
	if info, err := os.Stat(plain); err != nil || info.Size() == 0 {
		t.Errorf("the input was damaged: %v", err)
	}
}

func TestCompress_ForceReencodesASeekableInput(t *testing.T) {
	dir, plain, _, text := compressedInputFixture(t)
	coarse := filepath.Join(dir, "coarse.zst")
	if _, err := compressOne(t, compressParams{
		paths: []string{plain}, output: coarse, frameSize: "1M", level: 3, workers: 1,
	}); err != nil {
		t.Fatalf("write the seekable fixture: %v", err)
	}
	fine := filepath.Join(dir, "fine.zst")

	entry, err := compressOne(t, compressParams{
		paths: []string{coarse}, output: fine, frameSize: "16K", level: 3, workers: 1, force: true,
	})
	if err != nil {
		t.Fatalf("runCompress --force: %v (%v)", err, entry["error"])
	}
	if frames, _ := entry["frame_count"].(float64); frames < 10 {
		t.Errorf("frame_count: got %v, want the input split into 16K frames", entry["frame_count"])
	}
	if size, _ := entry["decompressed_size"].(float64); int64(size) != int64(len(text)) {
		t.Errorf("decompressed_size: got %v, want %d", entry["decompressed_size"], len(text))
	}
	if !strings.HasSuffix(fine, ".zst") || !seekable.IsSeekable(fine) {
		t.Error("the re-encoded output is not a seekable zstd file")
	}
}
