package trace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Every path a search passes over is named in skip_reasons with why, in
// the order skipped_files lists them: a binary file, UTF-16, a link the
// walk cannot resolve, a file and a subdirectory the process may not
// read, and a compressed stream that ends early, whose matches before the
// end are kept.
func TestTraceGivesTheReasonForEverySkippedPath(t *testing.T) {
	requireRipgrep(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads files of mode 000")
	}
	dir := t.TempDir()
	write := func(rel string, body []byte) string {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return path
	}
	write("app.log", []byte("LINE 1 NEEDLE\n"))
	binary := write("bin.dat", []byte("NEEDLE\x00\n"))
	utf16 := write("utf16.log", []byte{0xFF, 0xFE, 'N', 0, '\n', 0})
	dangling := filepath.Join(dir, "link.log")
	if err := os.Symlink("gone.log", dangling); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	locked := write("locked.log", []byte("NEEDLE\n"))
	gz := compressedcopy.Encode(t, compressedcopy.Gzip, []byte(strings.Repeat("LINE NEEDLE\n", 2000)))
	truncated := write("cut.log.gz", gz[:len(gz)/2])
	write("closed/inner.log", []byte("NEEDLE\n"))
	closed := filepath.Join(dir, "closed")
	for _, p := range []string{locked, closed} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
	}

	resp := traceOnce(t, dir, []string{"NEEDLE"}, Options{NoCache: true})

	want := map[string]string{
		binary:    "not a text file: a NUL byte in its first 8 KiB",
		utf16:     "not a text file: UTF-16 text, which rx does not decode",
		dangling:  "cannot resolve symlink",
		locked:    "permission denied",
		closed:    "permission denied",
		truncated: "not searched in full",
	}
	requireSkipReasons(t, resp, want)
	if !slices.ContainsFunc(resp.Matches, func(m rxtypes.Match) bool { return resp.Files[m.File] == truncated }) {
		t.Errorf("the matches before the end of %s were not kept", truncated)
	}
}

// requireSkipReasons checks that skip_reasons names exactly the paths of
// skipped_files, in the same order, each with a reason holding the
// wanted text.
func requireSkipReasons(t *testing.T, resp *rxtypes.TraceResponse, want map[string]string) {
	t.Helper()
	if resp.SkipReasons == nil {
		t.Fatal("skip_reasons is nil, which serializes as null")
	}
	paths := make([]string, len(resp.SkipReasons))
	for i, item := range resp.SkipReasons {
		paths[i] = item.Path
		if !strings.Contains(item.Reason, want[item.Path]) || want[item.Path] == "" {
			t.Errorf("%s: reason %q, want one holding %q", item.Path, item.Reason, want[item.Path])
		}
	}
	if !slices.Equal(paths, resp.SkippedFiles) {
		t.Errorf("skip_reasons paths %v, skipped_files %v: want the same list", paths, resp.SkippedFiles)
	}
	if len(paths) != len(want) {
		t.Errorf("skipped %v, want %d paths", paths, len(want))
	}
}

// A search that finds nothing to read still says why: the answer of the
// early return carries skip_reasons too, never null.
func TestTraceOfOnlySkippedFilesGivesTheirReasons(t *testing.T) {
	requireRipgrep(t)
	path := writeTextFile(t, "bin.dat", []byte("NEEDLE\x00\n"))
	resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
	requireSkipReasons(t, resp, map[string]string{path: "not a text file"})

	empty := t.TempDir()
	resp = traceOnce(t, empty, []string{"NEEDLE"}, Options{NoCache: true})
	if resp.SkipReasons == nil || len(resp.SkipReasons) != 0 {
		t.Errorf("skip_reasons of an empty directory: %#v, want []", resp.SkipReasons)
	}
}
