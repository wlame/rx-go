package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// largeFrameDir is a directory holding two seekable files of about
// 28 KB each whose one frame holds 256 MiB of text: one written by rx's
// encoder, whose frame header declares an 8 MiB window, and one whose
// single-segment frame declares the whole 256 MiB as its window.
func largeFrameDir(t *testing.T) string {
	t.Helper()
	const frameBytes = 256 << 20
	line := []byte("2025-12-10 07:00:00.000 INFO same line again\n")
	text := bytes.Repeat(line, frameBytes/len(line))

	var rxEncoded bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: len(text), Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &rxEncoded); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dir := t.TempDir()
	files := map[string][]byte{
		"rx-encoded.zst":     rxEncoded.Bytes(),
		"single-segment.zst": seekablefile.EncodeSingleSegment(t, [][]byte{text}),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// Listing a directory classifies every file in it. A file that declares
// a large frame is classified through a decoder that holds one bounded
// window, never the frame, so the listing costs a few mebibytes however
// large the frames its files declare. Before, each of these two files
// cost 256 MiB.
func TestTree_FilesDeclaringLargeFramesAreListedInAFewMebibytes(t *testing.T) {
	dir := largeFrameDir(t)
	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	var body struct {
		Entries []struct {
			Name              string  `json:"name"`
			IsText            *bool   `json:"is_text"`
			CompressionFormat *string `json:"compression_format"`
		} `json:"entries"`
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp, err := http.Get(ts.URL + "/v1/tree?path=" + dir) //nolint:noctx // test server
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("GET /v1/tree allocated %d KiB", allocated>>10)
	const budget = 32 << 20
	if allocated > budget {
		t.Errorf("GET /v1/tree allocated %d MiB for two files of about 28 KB; budget %d MiB",
			allocated>>20, budget>>20)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(body.Entries), body.Entries)
	}
	for _, entry := range body.Entries {
		if entry.CompressionFormat == nil || *entry.CompressionFormat != "zstd" {
			t.Errorf("%s: compression_format = %v, want zstd", entry.Name, entry.CompressionFormat)
		}
		if entry.IsText == nil || !*entry.IsText {
			t.Errorf("%s: is_text = %v, want true", entry.Name, entry.IsText)
		}
	}
}
