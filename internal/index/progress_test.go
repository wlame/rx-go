package index

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// progressText is a few hundred kilobytes of numbered lines, enough for
// several reads of every decoder.
func progressText() []byte {
	var buf bytes.Buffer
	for n := 1; n <= 8000; n++ {
		fmt.Fprintf(&buf, "LINE %d of the log, padded to give it some width\n", n)
	}
	return buf.Bytes()
}

// A build given a Progress reports how far it has read: nothing known
// before it starts, and the whole file once it is done, for every kind
// of file it indexes.
func TestBuild_ReportsProgressToTheEnd(t *testing.T) {
	text := progressText()
	dir := t.TempDir()
	files := map[string][]byte{"app.log": text}
	for name, format := range map[string]string{
		"app.log.gz": compressedcopy.Gzip, "app.log.xz": compressedcopy.Xz, "app.log.bz2": compressedcopy.Bzip2,
		"plain.log.zst": compressedcopy.Zstd,
	} {
		if encoded := compressedcopy.Encode(t, format, text); encoded != nil {
			files[name] = encoded
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	seekablePath := filepath.Join(dir, "app.log.zst")
	seekablefile.Write(t, seekablePath, seekablefile.SplitEvery(text, 64*1024))
	files["app.log.zst"] = nil

	for name := range files {
		for _, analyze := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s analyze=%v", name, analyze), func(t *testing.T) {
				progress := &Progress{}
				if _, known := progress.Fraction(); known {
					t.Fatal("a progress nobody started reports a fraction")
				}
				_, err := Build(filepath.Join(dir, name), BuildOptions{
					StepBytes: 4096, Analyze: analyze, Progress: progress,
				})
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				fraction, known := progress.Fraction()
				if !known || fraction != 1 {
					t.Fatalf("progress after the build = %v (known %v), want 1", fraction, known)
				}
			})
		}
	}
}

// Before the walk reads anything the build already knows how much there
// is to read, so a status request sees 0 rather than nothing.
func TestBuild_ProgressStartsAtZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, progressText(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	progress := &Progress{}
	var atStart float64
	var knownAtStart bool
	_, err := Build(path, BuildOptions{
		Progress:   progress,
		beforeWalk: func() { atStart, knownAtStart = progress.Fraction() },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !knownAtStart || atStart != 0 {
		t.Fatalf("progress before the walk = %v (known %v), want 0", atStart, knownAtStart)
	}
}

// Two identities are equal when every recorded field is; a missing
// field differs from a present one.
func TestSourceIdentity_Equal(t *testing.T) {
	inode, otherInode := uint64(7), uint64(8)
	fingerprint := "abc"
	base := SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Inode: &inode, Fingerprint: &fingerprint}
	cases := map[string]struct {
		other SourceIdentity
		equal bool
	}{
		"same values":        {SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Inode: &inode, Fingerprint: &fingerprint}, true},
		"another size":       {SourceIdentity{SizeBytes: 11, ModifiedAt: "t", Inode: &inode, Fingerprint: &fingerprint}, false},
		"another mtime":      {SourceIdentity{SizeBytes: 10, ModifiedAt: "u", Inode: &inode, Fingerprint: &fingerprint}, false},
		"another inode":      {SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Inode: &otherInode, Fingerprint: &fingerprint}, false},
		"no inode":           {SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Fingerprint: &fingerprint}, false},
		"no fingerprint":     {SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Inode: &inode}, false},
		"another changed at": {SourceIdentity{SizeBytes: 10, ModifiedAt: "t", Inode: &inode, ChangedAt: &fingerprint, Fingerprint: &fingerprint}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := base.Equal(tc.other); got != tc.equal {
				t.Fatalf("Equal = %v, want %v", got, tc.equal)
			}
		})
	}
}
