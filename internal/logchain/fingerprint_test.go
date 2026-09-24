package logchain

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fingerprintChain writes a chain of two frozen parts and an active
// file.
func fingerprintChain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.2", text: timedLines(chainBase, time.Second, 1, 5, "2"), mtime: chainBase.Add(time.Hour)},
		{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "1"), mtime: chainBase.Add(2 * time.Hour)},
		{name: "x.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 11, 5, "active"), mtime: chainBase.Add(3 * time.Hour)},
	})
	return dir
}

// appendTo adds text at the end of a file of dir.
func appendTo(t *testing.T, dir, name string, text []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The fingerprint stays while the active file grows, and changes with
// every other change a rotation or a writer makes to the files.
func TestFingerprint_ChangesWithTheFilesButNotWithGrowth(t *testing.T) {
	fingerprintOf := func(dir string) string { return Fingerprint(resolveIn(t, dir, "x.log")) }
	changes := []struct {
		label  string
		change func(t *testing.T, dir string)
	}{
		{"a rename", func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "x.log.2"), filepath.Join(dir, "x.log.3")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a compression", func(t *testing.T, dir string) {
			text, err := os.ReadFile(filepath.Join(dir, "x.log.1"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := os.Create(filepath.Join(dir, "x.log.1.gz"))
			if err != nil {
				t.Fatal(err)
			}
			w := gzip.NewWriter(out)
			_, _ = w.Write(text)
			_ = w.Close()
			_ = out.Close()
			if err := os.Remove(filepath.Join(dir, "x.log.1")); err != nil {
				t.Fatal(err)
			}
		}},
		{"the oldest part deleted", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "x.log.2")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a new part", func(t *testing.T, dir string) {
			writeChainFiles(t, dir, []chainFile{{name: "x.log.3", text: timedLines(chainBase.Add(-time.Hour), time.Second, 1, 2, "3")}})
		}},
		{"a write to a frozen part", func(t *testing.T, dir string) {
			appendTo(t, dir, "x.log.1", timedLines(chainBase.Add(time.Hour+time.Minute), time.Second, 99, 1, "1"))
		}},
		{"the active file replaced", func(t *testing.T, dir string) {
			fresh := filepath.Join(dir, "x.log.new")
			if err := os.WriteFile(fresh, timedLines(chainBase.Add(3*time.Hour), time.Second, 1, 1, "new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(fresh, filepath.Join(dir, "x.log")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range changes {
		t.Run(tc.label, func(t *testing.T) {
			dir := fingerprintChain(t)
			before := fingerprintOf(dir)
			if len(before) != 16 {
				t.Fatalf("fingerprint %q", before)
			}
			tc.change(t, dir)
			if after := fingerprintOf(dir); after == before {
				t.Fatalf("fingerprint %s unchanged", after)
			}
		})
	}
	t.Run("the active file grows", func(t *testing.T) {
		dir := fingerprintChain(t)
		before := fingerprintOf(dir)
		appendTo(t, dir, "x.log", timedLines(chainBase.Add(4*time.Hour), time.Second, 16, 3, "active"))
		if after := fingerprintOf(dir); after != before {
			t.Fatalf("fingerprint %s became %s", before, after)
		}
	})
}

// Lines appended to the active file between two describes leave the
// fingerprint and every part's global start as they were; only the
// active file's line count grows.
func TestDescribe_TheActiveFileGrowsBetweenTwoDescribes(t *testing.T) {
	dir := fingerprintChain(t)
	storeIndexes(t, dir, "x.log.2", "x.log.1")
	for round, opts := range []Options{{Scan: true}, {}} {
		before := describe(t, dir, "x.log", opts)
		// Each round appends lines an hour later than the last round's.
		later := chainBase.Add(time.Duration(4+round) * time.Hour)
		appendTo(t, dir, "x.log", timedLines(later, time.Second, 16+3*round, 3, "active"))
		after := describe(t, dir, "x.log", opts)
		if after.Response.Fingerprint != before.Response.Fingerprint || after.Response.State != rxtypes.ChainStateReady {
			t.Fatalf("scan %v: fingerprint %s then %s, state %s", opts.Scan, before.Response.Fingerprint,
				after.Response.Fingerprint, after.Response.State)
		}
		for k := range before.Response.Parts {
			if *before.Response.Parts[k].GlobalStart != *after.Response.Parts[k].GlobalStart {
				t.Fatalf("scan %v: %s moved", opts.Scan, before.Response.Parts[k].Name)
			}
		}
		active := len(after.Response.Parts) - 1
		if opts.Scan && *after.Response.Parts[active].LineCount != *before.Response.Parts[active].LineCount+3 {
			t.Fatalf("active line_count %d then %d", *before.Response.Parts[active].LineCount, *after.Response.Parts[active].LineCount)
		}
		if *after.Response.LastMs <= *before.Response.LastMs {
			t.Fatalf("scan %v: last_ms %d then %d", opts.Scan, *before.Response.LastMs, *after.Response.LastMs)
		}
	}
}
