package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// postCompress is a small helper: marshals req, POSTs it to the test
// server and returns the status code plus the decoded body.
func postCompress(t *testing.T, url string, req rxtypes.CompressRequest) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url+"/v1/compress", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// waitForFile polls for path to appear, up to a generous deadline. It
// returns true as soon as the file exists.
func waitForFile(path string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestCompress_OutputOutsideRootIsRejected asserts that an output_path
// outside the search roots is refused before any filesystem work: the
// server must not create the file and must not touch the input.
func TestCompress_OutputOutsideRootIsRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	input := filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	out := filepath.Join(outside, "x.zst")
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		OutputPath:       &out,
		FrameSize:        "4K",
		CompressionLevel: 3,
	})

	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %v)", status, body)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("output file %s was created outside the sandbox", out)
	}
}

// TestCompress_OutputOutsideRootViaSymlinkIsRejected covers a symlink
// that lives inside the root but points outside it: the sandbox must
// resolve the link before deciding.
func TestCompress_OutputOutsideRootViaSymlinkIsRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	input := filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	out := filepath.Join(link, "x.zst")
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		OutputPath:       &out,
		FrameSize:        "4K",
		CompressionLevel: 3,
	})

	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %v)", status, body)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.zst")); err == nil {
		t.Errorf("output file was created through the symlink")
	}
}

// TestCompress_OutputInsideRootIsAccepted is the positive control for
// the sandbox check: an in-root output still compresses.
func TestCompress_OutputInsideRootIsAccepted(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	out := filepath.Join(root, "custom.zst")
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		OutputPath:       &out,
		FrameSize:        "4K",
		CompressionLevel: 3,
	})

	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %v)", status, body)
	}
	if body["task_id"] == "" || body["task_id"] == nil {
		t.Errorf("no task_id in response: %v", body)
	}
	if !waitForFile(out) {
		t.Errorf("output file %s was never written", out)
	}
}

// TestCompress_DefaultOutputStaysInsideRoot asserts the derived
// default output (<input>.zst) is accepted and lands next to the input.
func TestCompress_DefaultOutputStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		FrameSize:        "4K",
		CompressionLevel: 3,
	})

	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %v)", status, body)
	}
	if !waitForFile(input + ".zst") {
		t.Errorf("default output %s.zst was never written", input)
	}
}

// TestCompress_ForceDoesNotOverwriteOutsideRoot is the arbitrary-write
// case: force=true must not let an out-of-sandbox file be truncated.
func TestCompress_ForceDoesNotOverwriteOutsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	input := filepath.Join(root, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	victim := filepath.Join(outside, "authorized_keys")
	original := []byte("ssh-ed25519 AAAA user@host\n")
	if err := os.WriteFile(victim, original, 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	before, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		OutputPath:       &victim,
		FrameSize:        "4K",
		CompressionLevel: 3,
		Force:            true,
	})

	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %v)", status, body)
	}
	// Give any (wrongly) spawned background task a chance to run before
	// asserting the victim file is intact.
	time.Sleep(200 * time.Millisecond)
	after, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("victim file disappeared: %v", err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("victim file was modified: size %d→%d, mtime %v→%v",
			before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if !bytes.Equal(content, original) {
		t.Errorf("victim content changed: %q", content)
	}
}

// TestCompress_InputOutsideRootIsRejected asserts the input path check
// answers with 403, not an unhandled error.
func TestCompress_InputOutsideRootIsRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	input := filepath.Join(outside, "a.log")
	if err := os.WriteFile(input, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	status, body := postCompress(t, ts.URL, rxtypes.CompressRequest{
		InputPath:        input,
		FrameSize:        "4K",
		CompressionLevel: 3,
	})
	if status != http.StatusForbidden {
		t.Errorf("status: got %d, want 403 (body: %v)", status, body)
	}
}
