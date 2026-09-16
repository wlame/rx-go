package webapi

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// compressInputServer starts a test server rooted at a fresh directory
// that holds app.log and its gzip copy.
func compressInputServer(t *testing.T) (url, root string, text []byte) {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root = t.TempDir()

	var body bytes.Buffer
	for n := 1; n <= 5000; n++ {
		fmt.Fprintf(&body, "LINE %d status=%d\n", n, 200+n%5)
	}
	text = body.Bytes()
	if err := os.WriteFile(filepath.Join(root, "app.log"), text, 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(text)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.log.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatalf("write gz: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return newTestServer(t).URL, root, text
}

// seekableText decompresses a seekable zstd file whole.
func seekableText(t *testing.T, path string) []byte {
	t.Helper()
	if !seekable.IsSeekable(path) {
		t.Fatalf("%s is not a seekable zstd file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	r, err := compression.NewReader(f, compression.FormatSeekableZstd)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = r.Close() }()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

// compressAndWait posts a compress request that must be accepted and
// returns the finished task's result.
func compressAndWait(t *testing.T, url string, req rxtypes.CompressRequest) map[string]any {
	t.Helper()
	status, created := postCompress(t, url, req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", status, created)
	}
	taskID, _ := created["task_id"].(string)
	body := waitForTaskBody(t, url, taskID)
	result, _ := body["result"].(map[string]any)
	return result
}

func TestCompressPost_GzipInputIsWrittenAsItsText(t *testing.T) {
	url, root, text := compressInputServer(t)
	output := filepath.Join(root, "from-gz.zst")
	noIndex := false

	result := compressAndWait(t, url, rxtypes.CompressRequest{
		InputPath: filepath.Join(root, "app.log.gz"), OutputPath: &output,
		FrameSize: "16K", CompressionLevel: 3, BuildIndex: &noIndex,
	})

	if got := seekableText(t, output); !bytes.Equal(got, text) {
		t.Errorf("output text differs from the decompressed input (%d vs %d bytes)", len(got), len(text))
	}
	if size, _ := result["decompressed_size"].(float64); int64(size) != int64(len(text)) {
		t.Errorf("decompressed_size: got %v, want %d", result["decompressed_size"], len(text))
	}
}

func TestCompressPost_RefusesWhatItCannotCompress(t *testing.T) {
	url, root, _ := compressInputServer(t)
	plain := filepath.Join(root, "app.log")
	seekableInput := filepath.Join(root, "already.zst")
	noIndex := false
	compressAndWait(t, url, rxtypes.CompressRequest{
		InputPath: plain, OutputPath: &seekableInput, FrameSize: "16K", CompressionLevel: 3, BuildIndex: &noIndex,
	})
	archive := filepath.Join(root, "logs.tar.gz")
	if err := os.WriteFile(archive, []byte("\x1f\x8b not really"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	again := filepath.Join(root, "again.zst")

	cases := []struct {
		name       string
		req        rxtypes.CompressRequest
		wantDetail string
	}{
		{"seekable zstd input",
			rxtypes.CompressRequest{InputPath: seekableInput, OutputPath: &again, FrameSize: "4M", CompressionLevel: 3},
			"already a seekable zstd file (set \"force\": true to re-encode it)"},
		{"compound archive",
			rxtypes.CompressRequest{InputPath: archive, FrameSize: "4M", CompressionLevel: 3},
			"compound archives (tar.gz, etc.) are not supported"},
		{"output is the input",
			rxtypes.CompressRequest{InputPath: plain, OutputPath: &plain, FrameSize: "4M", CompressionLevel: 3, Force: true},
			`the output path is the input file (set "output_path" to another file)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postCompress(t, url, tc.req)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %v", status, body)
			}
			detail, _ := body["detail"].(string)
			if !strings.HasSuffix(detail, tc.wantDetail) {
				t.Errorf("detail: got %q, want it to end with %q", detail, tc.wantDetail)
			}
		})
	}
	if _, err := os.Stat(again); err == nil {
		t.Error("a refused request still wrote its output")
	}
}

func TestCompressPost_ForceReencodesASeekableInput(t *testing.T) {
	url, root, text := compressInputServer(t)
	coarse := filepath.Join(root, "coarse.zst")
	fine := filepath.Join(root, "fine.zst")
	noIndex := false
	compressAndWait(t, url, rxtypes.CompressRequest{
		InputPath: filepath.Join(root, "app.log"), OutputPath: &coarse,
		FrameSize: "1M", CompressionLevel: 3, BuildIndex: &noIndex,
	})

	result := compressAndWait(t, url, rxtypes.CompressRequest{
		InputPath: coarse, OutputPath: &fine, FrameSize: "4K", CompressionLevel: 3, BuildIndex: &noIndex, Force: true,
	})

	if frames, _ := result["frame_count"].(float64); frames < 5 {
		t.Errorf("frame_count: got %v, want the text split into 4K frames", result["frame_count"])
	}
	if got := seekableText(t, fine); !bytes.Equal(got, text) {
		t.Error("re-encoded text differs from the input's text")
	}
}
