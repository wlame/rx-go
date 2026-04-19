package webapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// xzCompress compresses raw into xz format using the pure-Go ulikunitz/xz
// encoder. Returned bytes are a self-contained .xz stream.
//
// This is the same library the webapi decoder uses, so round-trip
// correctness is implicitly tested too.
func xzCompress(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz.NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("xz write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("xz close: %v", err)
	}
	return buf.Bytes()
}

// TestNewCompressedReader_XZRoundTrip verifies the in-package xz reader
// can decode a stream produced by the same library. Catches any future
// regression where the reader path is accidentally routed to subprocess
// or the stdlib fallback.
func TestNewCompressedReader_XZRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"single_line", "hello xz world\n"},
		{"multi_line", "line1\nline2\nline3\nline4\nline5\n"},
		{"binary_ish", string([]byte{0x00, 0x01, 0xff, 0xfe, 0x7f})},
		{"lorem", strings.Repeat("The quick brown fox jumps over the lazy dog.\n", 100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed := xzCompress(t, []byte(tc.content))
			r, err := newCompressedReader(bytes.NewReader(compressed), "xz")
			if err != nil {
				t.Fatalf("newCompressedReader: %v", err)
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.content {
				t.Errorf("round-trip mismatch: got %q, want %q",
					truncate(string(got), 80), truncate(tc.content, 80))
			}
		})
	}
}

// TestSamples_XZ_LineMode verifies GET /v1/samples?path=file.xz&lines=...
// returns the correctly-decoded content lines for an xz-compressed file.
//
// This is the main parity test for A2 — the xz file path was previously
// erroring with "xz decompression not supported".
func TestSamples_XZ_LineMode(t *testing.T) {
	root := t.TempDir()
	// Build a fixture with line numbers embedded so we can assert the
	// server returns the right lines.
	var rawBuf bytes.Buffer
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&rawBuf, "LOG-%03d: event line %d\n", i, i)
	}
	raw := rawBuf.Bytes()

	xzPath := filepath.Join(root, "events.log.xz")
	if err := os.WriteFile(xzPath, xzCompress(t, raw), 0o644); err != nil {
		t.Fatalf("write xz: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)

	// Request line 5 with zero context.
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=5&context=0", ts.URL, xzPath)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.IsCompressed {
		t.Error("IsCompressed should be true")
	}
	if out.CompressionFormat == nil || *out.CompressionFormat != "xz" {
		t.Errorf("compression_format: got %v, want xz", out.CompressionFormat)
	}
	lines := out.Samples["5"]
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	if lines[0] != "LOG-005: event line 5" {
		t.Errorf("line 5: got %q, want %q", lines[0], "LOG-005: event line 5")
	}
}

// TestSamples_XZ_RangeMode verifies range requests across an xz file.
func TestSamples_XZ_RangeMode(t *testing.T) {
	root := t.TempDir()
	var rawBuf bytes.Buffer
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&rawBuf, "row_%d\n", i)
	}
	xzPath := filepath.Join(root, "rows.log.xz")
	if err := os.WriteFile(xzPath, xzCompress(t, rawBuf.Bytes()), 0o644); err != nil {
		t.Fatalf("write xz: %v", err)
	}

	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	url := fmt.Sprintf("%s/v1/samples?path=%s&lines=10-12", ts.URL, xzPath)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Samples["10-12"]
	want := []string{"row_10", "row_11", "row_12"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSamples_XZ_ByteOffset_Rejected — byte-offset mode on xz must 400.
// Parity with gzip behavior: compressed files only allow line mode.
func TestSamples_XZ_ByteOffset_Rejected(t *testing.T) {
	root := t.TempDir()
	xzPath := filepath.Join(root, "a.log.xz")
	if err := os.WriteFile(xzPath, xzCompress(t, []byte("abcdef\n")), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&offsets=1", ts.URL, xzPath))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestSamples_XZ_NegativeIndex verifies -1 (last line) resolves correctly
// for xz files — exercises the streamCountLines helper on the xz path.
func TestSamples_XZ_NegativeIndex(t *testing.T) {
	root := t.TempDir()
	var rawBuf bytes.Buffer
	for i := 1; i <= 15; i++ {
		fmt.Fprintf(&rawBuf, "item=%d\n", i)
	}
	xzPath := filepath.Join(root, "items.log.xz")
	if err := os.WriteFile(xzPath, xzCompress(t, rawBuf.Bytes()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	ts := newTestServer(t)
	resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&lines=-1&context=0", ts.URL, xzPath))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out rxtypes.SamplesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Python parity: -1 resolves to len(lines), and the response uses
	// the resolved key. With 15 source lines, "-1" → key "15".
	got := out.Samples["15"]
	if len(got) != 1 {
		t.Fatalf("expected 1 line under key \"15\", got %d: full samples=%v",
			len(got), out.Samples)
	}
	if got[0] != "item=15" {
		t.Errorf("last line: got %q, want %q", got[0], "item=15")
	}
}

// truncate shortens a string to n runes with an ellipsis suffix.
// Used to keep test failure messages readable when asserting against
// large decompressed payloads.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
