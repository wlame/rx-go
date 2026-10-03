package trace

// A file is checked when the trace starts and read later, by the chunk
// workers, the decompressor, the frame scan or a cache reconstruction.
// A user who can write in a served directory can retarget a link, or
// replace a file with one, in between. These tests do exactly that at
// the last moment before the first read (Options.beforeRead) and hold
// the trace to reading only the file it checked: the file is skipped,
// and nothing of the new target comes back.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sandbox "github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// swapFixture is a sandboxed root holding a link app<ext> to real<ext>,
// and a file secret<ext> outside the root that the link is retargeted to.
type swapFixture struct {
	root   string
	link   string
	real   string
	secret string
}

// logBytes returns about size bytes of log lines, each holding word and
// NEEDLE. Random hex keeps a compressed copy above the 1 MB at which a
// compressed file's scan is cached.
func logBytes(word string, size int) []byte {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic fixture, not crypto
	var b strings.Builder
	for line := 1; b.Len() < size; line++ {
		fmt.Fprintf(&b, "line %d %s NEEDLE %016x%016x%016x%016x\n",
			line, word, rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return []byte(b.String())
}

// encodings writes text in one of the forms the engine reads.
var encodings = map[string]func(t *testing.T, path string, text []byte){
	".log": func(t *testing.T, path string, text []byte) {
		writeFile(t, path, text)
	},
	".log.gz": func(t *testing.T, path string, text []byte) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(text); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		writeFile(t, path, buf.Bytes())
	},
	".zst": func(t *testing.T, path string, text []byte) {
		encoded, err := os.ReadFile(writeSeekableZstdFile(t, text, 512<<10)) //nolint:gosec // test temp file
		if err != nil {
			t.Fatalf("read encoded: %v", err)
		}
		writeFile(t, path, encoded)
	},
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newSwapFixture builds the fixture for the encoding ext and confines
// the sandbox to its root.
func newSwapFixture(t *testing.T, ext string) swapFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	f := swapFixture{
		root:   filepath.Join(base, "root"),
		secret: filepath.Join(base, "outside", "secret"+ext),
	}
	f.link = filepath.Join(f.root, "app"+ext)
	f.real = filepath.Join(f.root, "real"+ext)
	for _, dir := range []string{f.root, filepath.Dir(f.secret)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	encodings[ext](t, f.real, logBytes("inside", 4<<20))
	encodings[ext](t, f.secret, logBytes("SECRET", 4<<20))
	if err := os.Symlink(filepath.Base(f.real), f.link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := sandbox.SetSearchRoots([]string{f.root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(sandbox.Reset)
	return f
}

// pointAt makes link a symbolic link to target, replacing whatever is
// at link.
func pointAt(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s: %v", link, err)
	}
}

// requireOnlyCheckedText fails when any match or context line of resp
// holds text of the file outside the root, and when swapped is not
// listed as skipped.
func requireOnlyCheckedText(t *testing.T, resp *rxtypes.TraceResponse, swapped string) {
	t.Helper()
	for _, m := range resp.Matches {
		if m.LineText != nil && strings.Contains(*m.LineText, "SECRET") {
			t.Fatalf("the trace read the retargeted file: %q from %s", *m.LineText, resp.Files[m.File])
		}
	}
	for _, lines := range resp.ContextLines {
		for _, line := range lines {
			if strings.Contains(line.LineText, "SECRET") {
				t.Fatalf("a context line comes from the retargeted file: %q", line.LineText)
			}
		}
	}
	if !slices.Contains(resp.SkippedFiles, swapped) {
		t.Errorf("skipped %v, want %s among them", resp.SkippedFiles, swapped)
	}
}

// Every way the engine reads a file: the chunk workers (plain), the
// decompressor (gzip), the frame scan (seekable zstd), and the cache
// reconstruction of either kind of cached scan.
func TestTrace_LinkRetargetedBeforeTheReadIsNotRead(t *testing.T) {
	cases := []struct {
		name   string
		ext    string
		cached bool
	}{
		{"plain", ".log", false},
		{"gzip", ".log.gz", false},
		{"seekable", ".zst", false},
		{"cached plain", ".log", true},
		{"cached seekable", ".zst", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			largeFileCacheEnv(t)
			f := newSwapFixture(t, tc.ext)
			patterns := []string{"NEEDLE"}
			opts := Options{ContextBefore: 1, ContextAfter: 1}
			if tc.cached {
				warm := traceOnce(t, f.link, patterns, opts)
				if len(warm.Matches) == 0 {
					t.Fatal("the warm-up trace found nothing")
				}
				opts.afterScan = func(string) { t.Fatal("the trace scanned the file instead of reading its cache") }
			}
			opts.beforeRead = func() { pointAt(t, f.link, f.secret) }

			resp := traceOnce(t, f.link, patterns, opts)

			requireOnlyCheckedText(t, resp, f.link)
		})
	}
}

// A link met in a directory walk and retargeted before the read.
func TestTrace_WalkedLinkRetargetedBeforeTheReadIsNotRead(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	f := newSwapFixture(t, ".log")
	opts := Options{beforeRead: func() { pointAt(t, f.link, f.secret) }}

	resp := traceOnce(t, f.root, []string{"NEEDLE"}, opts)

	requireOnlyCheckedText(t, resp, f.link)
	if !slices.Contains(mapValues(resp.Files), f.real) {
		t.Errorf("searched %v, want %s among them", resp.Files, f.real)
	}
}

// A named file replaced by a link after it was checked.
func TestTrace_NamedFileReplacedByALinkBeforeTheReadIsNotRead(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	f := newSwapFixture(t, ".log")
	opts := Options{beforeRead: func() { pointAt(t, f.real, f.secret) }}

	resp := traceOnce(t, f.real, []string{"NEEDLE"}, opts)

	requireOnlyCheckedText(t, resp, f.real)
}

// Line numbers a capped scan left unknown are counted from the file the
// trace checked, or left unknown: a retargeted link is not counted.
func TestResolveLinesByCounting_RefusesARetargetedLink(t *testing.T) {
	f := newSwapFixture(t, ".log")
	src, err := sandbox.Pin(f.link)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if lines := resolveLinesByCounting(src, []int64{0}); lines[0] != 1 {
		t.Fatalf("before the swap: %v, want offset 0 on line 1", lines)
	}

	pointAt(t, f.link, f.secret)

	if lines := resolveLinesByCounting(src, []int64{0}); len(lines) != 0 {
		t.Errorf("after the swap: %v, want nothing numbered", lines)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Checks the test's own premise: without a swap, every case reads the
// file and returns its lines.
func TestTrace_SwapFixtureIsSearchedWhenNothingChanges(t *testing.T) {
	largeFileCacheEnv(t)
	for ext := range encodings {
		f := newSwapFixture(t, ext)
		resp, err := New().RunWithOptions(context.Background(), []string{f.link}, []string{"NEEDLE"}, Options{})
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		if len(resp.Matches) == 0 || len(resp.SkippedFiles) != 0 {
			t.Errorf("%s: %d matches, skipped %v; want matches and nothing skipped", ext, len(resp.Matches), resp.SkippedFiles)
		}
	}
}

// pinForTest pins path for a test that calls a reader of the engine
// directly, as the engine pins every file before it reads it.
func pinForTest(t testing.TB, path string) sandbox.Pinned {
	t.Helper()
	src, err := sandbox.Pin(path)
	if err != nil {
		t.Fatalf("pin %s: %v", path, err)
	}
	return src
}
