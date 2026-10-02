package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/wlame/rx-go/internal/seekable"
)

// writeSeekableCopy compresses text into a seekable .zst at path with
// the repository's own encoder, in frames of frameSize bytes.
func writeSeekableCopy(t *testing.T, text []byte, path string, frameSize int) {
	t.Helper()
	dst, err := os.Create(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = dst.Close() }()
	enc := seekable.NewEncoder(seekable.EncoderConfig{FrameSize: frameSize, Level: 3, Workers: 1})
	if _, err := enc.Encode(context.Background(), bytes.NewReader(text), int64(len(text)), dst); err != nil {
		t.Fatalf("encode seekable: %v", err)
	}
}

// compressedCopies writes text in every compressed format rx-go reads
// and returns the paths by format name. bzip2 needs the bzip2 binary,
// since Go's standard library only decodes it; without the binary that
// format is left out.
func compressedCopies(t *testing.T, text []byte, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	write := func(name string, body []byte) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		out[name] = path
	}

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(text)
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	write("app.log.gz", gz.Bytes())

	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		t.Fatalf("xz writer: %v", err)
	}
	_, _ = xw.Write(text)
	if err := xw.Close(); err != nil {
		t.Fatalf("xz: %v", err)
	}
	write("app.log.xz", xzBuf.Bytes())

	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	write("app.log.zst", zw.EncodeAll(text, nil))
	_ = zw.Close()

	seekablePath := filepath.Join(dir, "app.seekable.zst")
	writeSeekableCopy(t, text, seekablePath, 512)
	out["app.seekable.zst"] = seekablePath

	if _, err := exec.LookPath("bzip2"); err == nil {
		cmd := exec.Command("bzip2", "-c")
		cmd.Stdin = bytes.NewReader(text)
		body, err := cmd.Output()
		if err != nil {
			t.Fatalf("bzip2: %v", err)
		}
		write("app.log.bz2", body)
	} else {
		t.Log("bzip2 binary not on PATH; the bz2 copy is not checked")
	}
	return out
}

// A frame that cannot be decoded fails the build of a seekable file
// whose text is walked for its timestamps, with an error, and the
// decoder and the walk that share the pass both stop. The text is
// longer than the head the timestamp format is detected from, and the
// damage lies past that head, so it is the shared pass that meets it.
func TestBuildOfACorruptSeekableFrameWithTimestampsFails(t *testing.T) {
	var text bytes.Buffer
	for i := 1; text.Len() < 3<<20; i++ {
		fmt.Fprintf(&text, "2026-10-11T10:%02d:%02dZ INFO LINE %d request handled\n", i/60%60, i%60, i)
	}
	zstPath := filepath.Join(t.TempDir(), "app.log.zst")
	writeSeekableCopy(t, text.Bytes(), zstPath, 64<<10)

	raw, err := os.ReadFile(zstPath) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Overwrite compressed frames three quarters of the way in; the seek
	// table at the end stays intact, so the file is still recognized as
	// seekable, and the head the format is detected from is untouched.
	damage := len(raw) * 3 / 4
	copy(raw[damage:], bytes.Repeat([]byte{0xff}, 64))
	if err := os.WriteFile(zstPath, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The build runs in its own goroutine so a pass that never ends
	// fails the test instead of hanging it; the channel has room for
	// the one result, so the goroutine can always deliver it and exit.
	done := make(chan error, 1)
	go func() {
		_, err := Build(zstPath, BuildOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Build of a corrupt seekable file succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Build of a corrupt seekable file did not return")
	}
}
