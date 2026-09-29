package index

import (
	"os"
	"path/filepath"
	"testing"
)

// A log that grows while it is indexed must not produce an index that
// covers more bytes than the size it records. The index describes the
// file as its first stat saw it: that size, those lines, and the
// identity taken then.
func TestBuild_GrowingFileIsIndexedAsItsFirstStatSawIt(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "growing.log")
	writeNumberedFile(t, path, 2000)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	identityBefore := IdentityFromInfo(path, before)

	idx, err := Build(path, BuildOptions{
		StepBytes: 4096,
		beforeWalk: func() {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open for append: %v", err)
			}
			defer func() { _ = f.Close() }()
			if _, err := f.WriteString("LINE 2001 appended during the build\n"); err != nil {
				t.Fatalf("append: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.SourceSizeBytes != before.Size() {
		t.Errorf("source_size_bytes = %d, want %d", idx.SourceSizeBytes, before.Size())
	}
	if idx.LineCount == nil || *idx.LineCount != 2000 {
		t.Errorf("line_count = %v, want the 2000 lines the first stat covered", idx.LineCount)
	}
	if last := idx.LineIndex[len(idx.LineIndex)-1]; last.ByteOffset >= before.Size() {
		t.Errorf("last checkpoint at %d, past the recorded size %d", last.ByteOffset, before.Size())
	}
	if idx.SourceFingerprint == nil || identityBefore.Fingerprint == nil ||
		*idx.SourceFingerprint != *identityBefore.Fingerprint {
		t.Errorf("fingerprint %v, want the one taken before the walk %v",
			idx.SourceFingerprint, identityBefore.Fingerprint)
	}
}
