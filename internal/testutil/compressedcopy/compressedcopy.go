// Package compressedcopy encodes a text in each compression format rx
// reads, so a test can write the same log as .gz, .bz2, .xz, .zst and
// seekable .zst and check that every copy answers like the plain file.
//
// This is test-only infrastructure. It lives under internal/testutil so
// production binaries never depend on it, and it must not be imported
// from non-test code.
package compressedcopy

import (
	"bytes"
	"compress/gzip"
	"context"
	"os/exec"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/seekable"
)

// Format names accepted by Encode.
const (
	Gzip         = "gzip"
	Bzip2        = "bzip2"
	Xz           = "xz"
	Zstd         = "zstd"
	SeekableZstd = "seekable zstd"
)

// Encode returns text encoded in format. It returns nil when the format
// needs a tool the host lacks: Go's standard library only decodes
// bzip2, so a bzip2 copy comes from the bzip2 binary. A caller skips
// its case on nil. An unknown format or a failing encoder fails t.
func Encode(t testing.TB, format string, text []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch format {
	case Gzip:
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(text)
		if err := w.Close(); err != nil {
			t.Fatalf("gzip: %v", err)
		}
	case Xz:
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatalf("xz writer: %v", err)
		}
		_, _ = w.Write(text)
		if err := w.Close(); err != nil {
			t.Fatalf("xz: %v", err)
		}
	case Zstd:
		// One plain zstd frame, without the seekable footer.
		w, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		buf.Write(w.EncodeAll(text, nil))
		_ = w.Close()
	case SeekableZstd:
		enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: 1024})
		if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), &buf); err != nil {
			t.Fatalf("seekable: %v", err)
		}
	case Bzip2:
		if _, err := exec.LookPath("bzip2"); err != nil {
			return nil
		}
		cmd := exec.Command("bzip2", "-c")
		cmd.Stdin = bytes.NewReader(text)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("bzip2: %v", err)
		}
		buf.Write(out)
	default:
		t.Fatalf("unknown format %q", format)
	}
	return buf.Bytes()
}
