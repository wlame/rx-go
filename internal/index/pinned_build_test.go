package index

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// Build checks its source as a named path before it reads a byte: a
// directory walk hands it paths that were checked earlier, and a link
// among them can be retargeted out of the search root in between.
func TestBuild_RefusesASourceOutsideTheSearchRoots(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(base, "secret.log")
	if err := os.WriteFile(secret, []byte("LINE 1 SECRET\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(root, "app.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)

	idx, err := Build(link, BuildOptions{})

	var outside *paths.ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("Build = %+v, %v; want ErrPathOutsideRoots", idx, err)
	}
}

// pinnedSourceKind is one kind of file Build reads, and how to write a
// file of that kind holding a given text.
type pinnedSourceKind struct {
	name    string
	ext     string
	analyze bool
	write   func(t *testing.T, path string, text []byte)
}

// pinnedSourceKinds covers each way Build reads its source: a plain
// walk, a stream through a decompressor, the frames of a seekable file,
// and the analysis pass of a plain and of a seekable file.
var pinnedSourceKinds = []pinnedSourceKind{
	{name: "plain", ext: ".log", write: writePlain},
	{name: "plain analyzed", ext: ".log", analyze: true, write: writePlain},
	{name: "gzip", ext: ".log.gz", write: writeGzip},
	{name: "seekable", ext: ".zst", write: writeSeekable},
	{name: "seekable analyzed", ext: ".zst", analyze: true, write: writeSeekable},
}

func writePlain(t *testing.T, path string, text []byte) {
	t.Helper()
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeGzip(t *testing.T, path string, text []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(text); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	writePlain(t, path, buf.Bytes())
}

func writeSeekable(t *testing.T, path string, text []byte) {
	t.Helper()
	writeSeekableCopy(t, text, path, 64)
}

// numberedText returns n lines that read `LINE <i> <word>`.
func numberedText(n int, word string) []byte {
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&buf, "LINE %d %s\n", i, word)
	}
	return buf.Bytes()
}

// pinnedSourceFixture is a search root holding app<ext>, a link to
// real<ext> beside it (3 lines), and a secret<ext> of 7 lines outside
// the root that the link can be retargeted to.
type pinnedSourceFixture struct {
	link, real string
	// retarget points the link at the secret, as a user who can write
	// in the root would.
	retarget func()
}

func newPinnedSourceFixture(t *testing.T, kind pinnedSourceKind) pinnedSourceFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	real := filepath.Join(root, "real"+kind.ext)
	kind.write(t, real, numberedText(3, "REAL"))
	kind.write(t, filepath.Join(outside, "secret"+kind.ext), numberedText(7, "SECRET"))
	link := filepath.Join(root, "app"+kind.ext)
	if err := os.Symlink("real"+kind.ext, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return pinnedSourceFixture{
		link: link,
		real: real,
		retarget: func() {
			if err := os.Remove(link); err != nil {
				t.Errorf("remove link: %v", err)
			}
			target := filepath.Join("..", "outside", "secret"+kind.ext)
			if err := os.Symlink(target, link); err != nil {
				t.Errorf("symlink: %v", err)
			}
		},
	}
}

// A link retargeted out of the root after Build pinned its source and
// before Build opened it: the build is refused, whatever kind of file it
// is, and no index of the outside file comes back.
func TestBuild_RefusesASourceRetargetedAfterThePin(t *testing.T) {
	for _, kind := range pinnedSourceKinds {
		t.Run(kind.name, func(t *testing.T) {
			fx := newPinnedSourceFixture(t, kind)

			idx, err := Build(fx.link, BuildOptions{Analyze: kind.analyze, afterPin: fx.retarget})

			if !errors.Is(err, paths.ErrFileChanged) {
				t.Errorf("Build = %v; want ErrFileChanged", err)
				if idx != nil && idx.LineCount != nil {
					t.Errorf("index of %d lines came back", *idx.LineCount)
				}
			}
		})
	}
}

// A link retargeted after Build opened its source: every read of the
// build, the fingerprint included, goes through the handle it opened,
// so the index describes the file that was pinned, not the outside one
// the path now leads to.
func TestBuild_ReadsOnlyThePinnedFileAfterItIsOpened(t *testing.T) {
	for _, kind := range pinnedSourceKinds {
		t.Run(kind.name, func(t *testing.T) {
			fx := newPinnedSourceFixture(t, kind)

			idx, err := Build(fx.link, BuildOptions{Analyze: kind.analyze, beforeWalk: fx.retarget})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			if idx.LineCount == nil || *idx.LineCount != 3 {
				t.Errorf("line_count = %s, want the 3 lines of the pinned file", lineCountText(idx.LineCount))
			}
			want, err := SourceFingerprint(fx.real)
			if err != nil {
				t.Fatalf("SourceFingerprint: %v", err)
			}
			if idx.SourceFingerprint == nil || *idx.SourceFingerprint != want {
				t.Errorf("source_fingerprint = %v, want the pinned file's %s", idx.SourceFingerprint, want)
			}
		})
	}
}

// lineCountText prints an index's line count, which may be absent.
func lineCountText(count *int64) string {
	if count == nil {
		return "null"
	}
	return fmt.Sprint(*count)
}
