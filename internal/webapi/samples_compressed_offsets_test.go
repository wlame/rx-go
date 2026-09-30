package webapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// GET /v1/samples answers a byte offset in a compressed file with the
// line that holds it, as it does for the plain copy of the same text:
// the offset is a position in the decompressed stream, the coordinate a
// trace of the file reports. The first request builds the file's index
// and the second reads it; both give the same answer.
func TestSamples_OffsetsInACompressedFileAnswerAsThePlainFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	var text bytes.Buffer
	var target int64
	for n := 1; n <= 400; n++ {
		if n == 321 {
			target = int64(text.Len()) + 4
		}
		fmt.Fprintf(&text, "LINE %d of the log\n", n)
	}
	plainPath := filepath.Join(root, "app.log")
	if err := os.WriteFile(plainPath, text.Bytes(), 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(text.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	gzPath := plainPath + ".gz"
	if err := os.WriteFile(gzPath, gz.Bytes(), 0o600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}

	ts := newTestServer(t)
	get := func(path string) rxtypes.SamplesResponse {
		t.Helper()
		query := url.Values{"path": {path}, "offsets": {strconv.FormatInt(target, 10)}, "context": {"1"}}
		resp, err := http.Get(ts.URL + "/v1/samples?" + query.Encode())
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", path, resp.StatusCode)
		}
		var body rxtypes.SamplesResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	key := strconv.FormatInt(target, 10)
	plain := get(plainPath)
	if plain.Offsets[key] != 321 {
		t.Fatalf("plain: offset %d is on line %d, want 321", target, plain.Offsets[key])
	}
	for _, run := range []string{"cache empty", "index built"} {
		got := get(gzPath)
		if !got.IsCompressed {
			t.Errorf("%s: the answer does not say the file is compressed", run)
		}
		if !reflect.DeepEqual(got.Offsets, plain.Offsets) || !reflect.DeepEqual(got.Samples, plain.Samples) {
			t.Errorf("%s: gzip answered %v %v, plain %v %v", run, got.Offsets, got.Samples, plain.Offsets, plain.Samples)
		}
	}
}
