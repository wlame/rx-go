package filekind

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// logText is a few lines of plain log text.
const logText = "LINE 1 WARN worker started\nLINE 2 ERROR disk full\nLINE 3 INFO done\n"

func gzipOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(body)
	if err := w.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Bytes()
}

func zstdOf(t *testing.T, body []byte) []byte {
	t.Helper()
	w, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = w.Close() }()
	return w.EncodeAll(body, nil)
}

func seekableOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 16, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(body), int64(len(body)), &buf); err != nil {
		t.Fatalf("seekable: %v", err)
	}
	return buf.Bytes()
}

// tarGzOf is a .tar.gz holding one file of body.
func tarGzOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "app.log", Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	_, _ = tw.Write(body)
	if err := tw.Close(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	return gzipOf(t, tarBuf.Bytes())
}

// pzstdOf is zstd text behind a skippable frame, the way pzstd writes
// it: the file starts with the skippable frame's magic, not zstd's.
func pzstdOf(t *testing.T, body []byte) []byte {
	t.Helper()
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 4, 0, 0, 0, 1, 2, 3, 4}
	return append(skippable, zstdOf(t, body)...)
}

// The one rule, over the files that used to be classified differently
// by each command: the signature decides the format, whatever the name,
// and the first 8 KiB of the text decide whether it is text.
func TestOf_DecidesFormatFromTheBytesAndTextFromTheText(t *testing.T) {
	utf16 := []byte{0xFE, 0xFF, 0, 'L', 0, 'I', 0, 'N', 0, 'E', 0, '\n'}
	markWithoutNUL := append([]byte{0xFF, 0xFE}, []byte(" ERR bad\n")...)
	nulLate := append(bytes.Repeat([]byte("x\n"), TextProbeBytes/2), 0, '\n')
	nulEarly := append([]byte("LINE 1\n"), 0, '\n')
	nulAtTheEdge := append(bytes.Repeat([]byte("x"), TextProbeBytes-1), 0)
	tests := []struct {
		name       string
		body       []byte
		wantFormat compression.Format
		wantReason string
	}{
		{"plain.log", []byte(logText), compression.FormatNone, ""},
		{"empty.log", nil, compression.FormatNone, ""},
		{"text-named.gz", []byte(logText), compression.FormatNone, ""},
		{"gzip-named.log", gzipOf(t, []byte(logText)), compression.FormatGzip, ""},
		{"zstd-named.gz", zstdOf(t, []byte(logText)), compression.FormatZstd, ""},
		{"pzstd.zst", pzstdOf(t, []byte(logText)), compression.FormatZstd, ""},
		{"seekable.log", seekableOf(t, []byte(logText)), compression.FormatSeekableZstd, ""},
		{"empty-seekable.zst", seekableOf(t, nil), compression.FormatSeekableZstd, ""},
		{"empty.gz", gzipOf(t, nil), compression.FormatGzip, ""},
		{"nul-early.log", nulEarly, compression.FormatNone, notTextReasons[nulByte][0]},
		{"nul-after-8k.log", nulLate, compression.FormatNone, ""},
		{"nul-last-of-8k.log", nulAtTheEdge, compression.FormatNone, notTextReasons[nulByte][0]},
		{"high-bytes.log", bytes.Repeat([]byte{0xC3, 0xA9, '\n'}, 100), compression.FormatNone, ""},
		{"nul-early.log.gz", gzipOf(t, nulEarly), compression.FormatGzip, notTextReasons[nulByte][1]},
		{"nul-early.log.zst", seekableOf(t, nulEarly), compression.FormatSeekableZstd, notTextReasons[nulByte][1]},
		{"archive.tar.gz", tarGzOf(t, []byte(logText)), compression.FormatGzip, notTextReasons[nulByte][1]},
		{"text-named.tar.gz", []byte(logText), compression.FormatNone, ""},
		{"utf16.log", utf16, compression.FormatNone, notTextReasons[utf16Text][0]},
		{"invalid-utf8-starting-ff-fe.log", markWithoutNUL, compression.FormatNone, ""},
		{"utf16.log.gz", gzipOf(t, utf16), compression.FormatGzip, notTextReasons[utf16Text][1]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind := Of(bytes.NewReader(tc.body), int64(len(tc.body)))
			if kind.Format != tc.wantFormat {
				t.Errorf("format: got %q, want %q", kind.Format, tc.wantFormat)
			}
			if kind.NotText != tc.wantReason {
				t.Errorf("not text: got %q, want %q", kind.NotText, tc.wantReason)
			}
			if kind.IsText() != (tc.wantReason == "") {
				t.Errorf("IsText: got %v with reason %q", kind.IsText(), kind.NotText)
			}
			if (kind.Table != nil) != kind.IsSeekable() {
				t.Errorf("table %v for format %q", kind.Table, kind.Format)
			}
		})
	}
}

func TestNotTextReasonsStartWithThePrefix(t *testing.T) {
	for flaw, wordings := range notTextReasons {
		for _, reason := range wordings {
			if !strings.HasPrefix(reason, NotTextPrefix) {
				t.Errorf("flaw %d: %q does not start with %q", flaw, reason, NotTextPrefix)
			}
		}
	}
}

// A seekable zstd file reports "zstd" as its compression, what any
// other tool calls it; a plain file reports none.
func TestKind_CompressionName(t *testing.T) {
	cases := map[compression.Format]string{
		compression.FormatNone:         "",
		compression.FormatGzip:         "gzip",
		compression.FormatZstd:         "zstd",
		compression.FormatSeekableZstd: "zstd",
	}
	for format, want := range cases {
		if got := (Kind{Format: format}).CompressionName(); got != want {
			t.Errorf("%q: got %q, want %q", format, got, want)
		}
	}
}

// A file the process may not read is an error from OfPinned, never a
// Kind: the caller reports it as a file it cannot read, not as binary.
func TestOfPinned_UnreadableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	path := filepath.Join(t.TempDir(), "locked.log")
	if err := os.WriteFile(path, []byte(logText), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src, err := paths.Pin(path)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := OfPinned(src); !os.IsPermission(err) {
		t.Errorf("got %v, want a permission error", err)
	}
}
