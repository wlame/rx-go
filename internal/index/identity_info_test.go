package index

import (
	"os"
	"path/filepath"
	"testing"
)

// MatchesInfo compares what one stat gives: the file as it was matches;
// a rewrite that keeps the size and puts the mtime back does not (its
// ctime moved), and neither does another file renamed into the name.
// The identity an index recorded is RecordedIdentity.
func TestSourceIdentity_MatchesInfo(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	writeNumberedFile(t, src, 100)
	idx := buildIndexFor(t, src)
	recorded := RecordedIdentity(idx)
	stat := func() os.FileInfo {
		t.Helper()
		info, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	before := stat()
	if !recorded.MatchesInfo(before) || !StatIdentity(before).MatchesInfo(before) {
		t.Fatal("an unchanged file does not match")
	}

	raw, err := os.ReadFile(src) //nolint:gosec // fixture path from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	raw[len("LINE 1")] = '\n'
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	rewritten := stat()
	if rewritten.Size() != before.Size() || !rewritten.ModTime().Equal(before.ModTime()) {
		t.Fatal("the rewrite moved the size or the mtime; the test proves nothing")
	}
	if _, ok := sourceIdentity(rewritten); ok && recorded.MatchesInfo(rewritten) {
		t.Error("a rewrite in place still matches")
	}

	other := filepath.Join(dir, "other.log")
	writeNumberedFile(t, other, 100)
	if err := os.Rename(other, src); err != nil {
		t.Fatal(err)
	}
	if _, ok := sourceIdentity(before); ok && StatIdentity(before).MatchesInfo(stat()) {
		t.Error("another file renamed into the name still matches")
	}
}
