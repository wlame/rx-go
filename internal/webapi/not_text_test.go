package webapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/xzfile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A file whose text is not text has no lines: GET /v1/samples and POST
// /v1/index both refuse it with 400 and the reason, as the CLI does,
// and no index task is started for it.
func TestNotTextFileIsRefusedBySamplesAndIndex(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	archive := filepath.Join(root, "logs.tar.gz")
	body := compressedcopy.Encode(t, compressedcopy.Gzip, append([]byte("app.log"), make([]byte, 505)...))
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ts := newTestServer(t)
	const reason = "not a text file: a NUL byte in the first 8 KiB of its decompressed text"

	resp, err := http.Get(ts.URL + "/v1/samples?path=" + url.QueryEscape(archive) + "&lines=1&context=0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, reason)

	zero := 0
	payload, _ := json.Marshal(rxtypes.IndexRequest{Path: archive, Threshold: &zero})
	resp, err = http.Post(ts.URL+"/v1/index", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, reason)
}

// requireRefusal checks resp has the status and a body that names why.
func requireRefusal(t *testing.T, resp *http.Response, status int, why string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status || !strings.Contains(string(body), why) {
		t.Errorf("got %d %s, want %d naming %q", resp.StatusCode, body, status, why)
	}
}

// A file rx refuses to decompress, here an xz file whose block declares
// a 256 MiB dictionary, is refused by GET /v1/samples with 400 and the
// reason, like a file that is not text: the file is at fault, not the
// server.
func TestSamplesRefusesAFileThatNeedsMoreThanTheLimitToDecode(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	path := filepath.Join(root, "big-dictionary.log.xz")
	text := bytes.Repeat([]byte("2025-12-10 07:00:00.000 INFO a line\n"), 1000)
	if err := os.WriteFile(path, xzfile.WithDictionaryCode(t, xzfile.Encode(t, text, xz.WriterConfig{}), 0, 32), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/samples?path=" + url.QueryEscape(path) + "&lines=1&context=0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	requireRefusal(t, resp, http.StatusBadRequest, compression.TooLargeToDecodeReason)
}
