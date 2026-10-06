package samples

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// pinForTest pins path for a test that calls a reader of this package
// directly, as Resolve pins the file before it reads it.
func pinForTest(t testing.TB, path string) paths.Pinned {
	t.Helper()
	src, err := paths.Pin(path)
	if err != nil {
		t.Fatalf("pin %s: %v", path, err)
	}
	return src
}

// linkFixture is a sandboxed root holding app<ext> -> real<ext>, and a
// file outside the root that the link is retargeted to after the check.
type linkFixture struct {
	link, secret string
}

func newLinkFixture(t *testing.T, ext string, encode func([]byte) []byte) linkFixture {
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
	f := linkFixture{link: filepath.Join(root, "app"+ext), secret: filepath.Join(outside, "secret"+ext)}
	writes := map[string]string{
		filepath.Join(root, "real"+ext): "LINE 1 inside\nLINE 2 inside\n",
		f.secret:                        "LINE 1 SECRET\nLINE 2 SECRET\n",
	}
	for path, text := range writes {
		if err := os.WriteFile(path, encode([]byte(text)), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := os.Symlink("real"+ext, f.link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return f
}

func gzipBytes(text []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(text)
	_ = zw.Close()
	return buf.Bytes()
}

// A caller checks a path, and a link on it is retargeted before the
// read: samples reads nothing of the new target, by line or by offset,
// plain or compressed.
func TestResolve_RefusesALinkRetargetedAfterTheCheck(t *testing.T) {
	plain := func(b []byte) []byte { return b }
	cases := []struct {
		name   string
		ext    string
		encode func([]byte) []byte
		req    Request
	}{
		{"plain lines", ".log", plain, Request{Lines: []OffsetOrRange{{Start: 1}}}},
		{"plain offsets", ".log", plain, Request{Offsets: []OffsetOrRange{{Start: 0}}}},
		{"gzip lines", ".log.gz", gzipBytes, Request{Lines: []OffsetOrRange{{Start: 1}}}},
		{"gzip offsets", ".log.gz", gzipBytes, Request{Offsets: []OffsetOrRange{{Start: 0}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, tc.ext, tc.encode)
			req := tc.req
			req.Path = f.link
			req.Source = pinForTest(t, f.link)
			req.IndexLoader = NoIndex

			if err := os.Remove(f.link); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if err := os.Symlink(f.secret, f.link); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			resp, err := Resolve(t.Context(), req)
			if !errors.Is(err, paths.ErrFileChanged) {
				t.Errorf("Resolve = %+v, %v; want ErrFileChanged", resp, err)
			}
		})
	}
}

// Without a pin from the caller, Resolve pins the path itself, with the
// same check a caller makes: a link out of the root is refused.
func TestResolve_PinsAnUncheckedPathItself(t *testing.T) {
	f := newLinkFixture(t, ".log", func(b []byte) []byte { return b })
	outsideLink := filepath.Join(filepath.Dir(f.link), "out.log")
	if err := os.Symlink(f.secret, outsideLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	resp, err := Resolve(t.Context(), Request{Path: outsideLink, Lines: []OffsetOrRange{{Start: 1}}, IndexLoader: NoIndex})

	var outside *paths.ErrPathOutsideRoots
	if !errors.As(err, &outside) {
		t.Errorf("Resolve = %+v, %v; want ErrPathOutsideRoots", resp, err)
	}
}
