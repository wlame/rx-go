package testparity

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Well-known fixture names. These match the files we expect under
// testdata/fixtures/. Any test using a name not in this list should
// document the fixture inline.
const (
	FixtureTiny   = "tiny.log"
	FixtureMedium = "medium.log"
	FixtureLarge  = "large.log" // generated lazily — large binary
	FixtureBinary = "binary.dat"
)

// CompressedFormats lists the variants produced by EnsureCompressedFixtures.
// These are the formats rx-go's compressed path needs to handle; the
// seekable-zstd variant lives in internal/seekable tests.
var CompressedFormats = []string{"gz", "bz2", "xz", "zst"}

var (
	fixturesOnce sync.Once
	fixturesErr  error
)

// EnsureFixtures builds the tiny / medium / binary fixtures on disk if
// they aren't already present. Safe to call from multiple tests —
// sync.Once serializes the build. Large-file generation is opt-in via
// EnsureLargeFixture.
func EnsureFixtures(t *testing.T) {
	t.Helper()
	fixturesOnce.Do(func() {
		fixturesErr = buildBaseFixtures()
	})
	if fixturesErr != nil {
		t.Fatalf("fixture generation: %v", fixturesErr)
	}
}

// buildBaseFixtures writes the small-and-medium fixtures under testdata/fixtures/.
// Called once per test process via EnsureFixtures.
func buildBaseFixtures() error {
	dir := FixturesDir()
	// Test-fixtures directory; 0o750 is the strictest mode that still
	// lets `go test -run` in a CI container read the fixtures back.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Tiny: 14 lines, mix of content and blanks.
	tiny := []byte("line 1 hello\nline 2 world\nline 3\n\nline 5 hello again\n" +
		"line 6 goodbye\n\nline 8 error occurred\n" +
		"line 9 ok\nline 10 hello\nline 11 world\nline 12 error\n" +
		"line 13 final\nline 14\n")
	if err := writeIfAbsent(filepath.Join(dir, FixtureTiny), tiny); err != nil {
		return err
	}

	// Medium: ~4 MB of plausible log lines.
	var medium bytes.Buffer
	for i := 1; medium.Len() < 4*1024*1024; i++ {
		fmt.Fprintf(&medium, "2026-04-18T%02d:%02d:%02d.%03d INFO request=%d status=200 path=/api/v1/items/%d latency=%dms\n",
			i%24, (i*7)%60, (i*13)%60, i%1000, i, i%500, i%200)
		if i%173 == 0 {
			fmt.Fprintf(&medium, "2026-04-18T%02d:%02d:%02d.000 ERROR request=%d exception=%s\n",
				i%24, (i*7)%60, (i*13)%60, i, "ConnectionRefused")
		}
	}
	if err := writeIfAbsent(filepath.Join(dir, FixtureMedium), medium.Bytes()); err != nil {
		return err
	}

	// Binary fixture: bytes 0..255 repeated a few times.
	var bin bytes.Buffer
	for i := 0; i < 4; i++ {
		for b := 0; b < 256; b++ {
			bin.WriteByte(byte(b))
		}
	}
	return writeIfAbsent(filepath.Join(dir, FixtureBinary), bin.Bytes())
}

// writeIfAbsent only writes `path` when it doesn't already exist. Keeps
// test reruns fast and avoids touching git-tracked files that happen to
// be identical.
func writeIfAbsent(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, data, 0o600)
}

// EnsureLargeFixture generates the ~100 MB large.log if missing. This
// is opt-in — do NOT call unconditionally from test setup.
//
// Content is procedurally generated so the file can be rebuilt
// reproducibly. Not checked into git.
func EnsureLargeFixture(t *testing.T) string {
	t.Helper()
	EnsureFixtures(t)
	path := filepath.Join(FixturesDir(), FixtureLarge)
	if info, err := os.Stat(path); err == nil && info.Size() > 80*1024*1024 {
		return path
	}
	// Build it. ~100 MB of log lines.
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create large fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	bw := make([]byte, 0, 256)
	const target = 100 * 1024 * 1024
	var written int
	for i := 1; written < target; i++ {
		bw = bw[:0]
		bw = fmt.Appendf(bw, "2026-04-18T%02d:%02d:%02d.%03d INFO request=%d status=200 path=/api/v1/items/%d latency=%dms user=user-%d\n",
			i%24, (i*7)%60, (i*13)%60, i%1000, i, i%500, i%200, i%50)
		if _, err := f.Write(bw); err != nil {
			t.Fatalf("write large fixture: %v", err)
		}
		written += len(bw)
	}
	return path
}

// EnsureCompressedFixtures writes the common compressed variants of a
// source file to sibling paths (foo.log.gz, foo.log.xz, etc.) if they
// don't exist. Returns a map from format name → path.
//
// Built on demand because some tests only need specific formats.
func EnsureCompressedFixtures(t *testing.T, source string) map[string]string {
	t.Helper()
	EnsureFixtures(t)
	srcBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	result := map[string]string{}
	for _, fmtName := range CompressedFormats {
		target := source + "." + fmtName
		result[fmtName] = target
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := compressFile(srcBytes, target, fmtName); err != nil {
			t.Fatalf("compress %s: %v", fmtName, err)
		}
	}
	return result
}

// compressFile writes `data` compressed as the named format to `target`.
func compressFile(data []byte, target, format string) error {
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var w io.WriteCloser
	switch format {
	case "gz":
		w = gzip.NewWriter(f)
	case "bz2":
		// bz2 stdlib has no writer; use a minimal external shell-out.
		return writeBz2(data, target)
	case "xz":
		xw, err := xz.NewWriter(f)
		if err != nil {
			return err
		}
		w = xw
	case "zst":
		zw, err := zstd.NewWriter(f)
		if err != nil {
			return err
		}
		w = zw
	default:
		return fmt.Errorf("unknown format %s", format)
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// writeBz2 compresses `data` via the system `bzip2` binary. Falls back
// gracefully: if bzip2 isn't installed, the compressed-path tests that
// use bz2 will skip. The bzip2 reader is in the Go stdlib but the
// writer is not — hence this subprocess fallback.
func writeBz2(data []byte, target string) error {
	cmd := bzip2Command(target)
	if cmd == nil {
		return fmt.Errorf("bzip2 binary not found")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_, _ = stdin.Write(data)
		_ = stdin.Close()
	}()
	return cmd.Wait()
}

// VerifyBz2ReadBack sanity-checks that the compressed file round-trips.
// Used by the fixture builder's tests to catch corrupted writes.
//
//nolint:unused // utility provided for tests that want to explicitly verify.
func VerifyBz2ReadBack(t *testing.T, path string, wantBytes []byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	r := bzip2.NewReader(f)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("bzip2 read: %v", err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Errorf("bz2 round-trip mismatch: got %d bytes, want %d", len(got), len(wantBytes))
	}
}
