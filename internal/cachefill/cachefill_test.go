package cachefill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
)

// The listing is the walk's: the same paths in the same order as
// paths.WalkPinned gives, hidden entries left out, a refused link kept
// as a skip with the walk's reason; a binary file is listed (what it is
// is decided when it is reached). Without recursion only the top level
// is listed.
func TestList_IsTheWalkOfTheTreesRules(t *testing.T) {
	dir := folder(t)
	outside := filepath.Join(t.TempDir(), "x.log")
	writeFile(t, outside, "ERROR outside\n")
	if err := os.Symlink(outside, filepath.Join(dir, "out.log")); err != nil {
		t.Fatal(err)
	}
	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)

	for _, recursive := range []bool{true, false} {
		entries, err := List(pin(t, dir), recursive)
		if err != nil {
			t.Fatal(err)
		}
		walk, err := paths.WalkPinned(pin(t, dir), recursive)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(walk) {
			t.Fatalf("recursive=%v: %d entries, the walk reports %d", recursive, len(entries), len(walk))
		}
		var listed []string
		for i, entry := range entries {
			listed = append(listed, filepath.Base(entry.Path))
			if entry.Path != walk[i].Path || entry.Skip != walk[i].Refused || entry.Named {
				t.Errorf("recursive=%v entry %d: %+v, the walk reports %+v", recursive, i, entry, walk[i])
			}
		}
		if slices.Contains(listed, ".secret.log") {
			t.Errorf("recursive=%v: a hidden file is listed: %v", recursive, listed)
		}
		if !slices.Contains(listed, "blob.bin") || !slices.Contains(listed, "out.log") {
			t.Errorf("recursive=%v: listing %v", recursive, listed)
		}
		if got := slices.Contains(listed, "c.log"); got != recursive {
			t.Errorf("recursive=%v: sub/deeper/c.log listed=%v", recursive, got)
		}
	}
}

// A directory below the listed one that cannot be listed is a skip with
// the reason, and the rest of the tree is listed.
func TestList_AnUnlistableSubdirectoryIsASkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists every directory")
	}
	dir := folder(t)
	locked := filepath.Join(dir, "sub", "deeper")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	entries, err := List(pin(t, dir), true)
	if err != nil {
		t.Fatal(err)
	}
	var skipped, files []string
	for _, entry := range entries {
		if entry.Skip != "" {
			skipped = append(skipped, entry.Path+": "+entry.Skip)
			continue
		}
		files = append(files, filepath.Base(entry.Path))
	}
	if len(skipped) != 1 || skipped[0] != locked+": "+paths.ReasonPermissionDenied {
		t.Errorf("skips %v, want the locked directory", skipped)
	}
	if !slices.Equal(files, []string{"app.log", "blob.bin", "b.log"}) {
		t.Errorf("files %v", files)
	}
}

// Each fills the files one at a time in the listing's order, records a
// walk's skip without filling it, counts files_done as it goes, and
// stops between files once ctx is canceled: the file whose fill saw the
// cancel is not recorded, and no later file is filled.
func TestEach_FillsInOrderCountsAndStopsBetweenFilesOnCancel(t *testing.T) {
	entries := []Entry{{Path: "/d/a.log"}, {Path: "/d/link", Skip: "refused"}, {Path: "/d/b.log"}, {Path: "/d/c.log"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := &Progress{}
	var filled, recorded []string
	var doneSeen []int64
	err := Each(ctx, entries, progress, func(_ context.Context, e Entry) FileResult {
		done, total := progress.Files()
		if total != int64(len(entries)) {
			t.Errorf("files_total %d during %s", total, e.Path)
		}
		doneSeen = append(doneSeen, done)
		filled = append(filled, e.Path)
		if e.Path == "/d/b.log" {
			cancel() // a DELETE while b.log is filled
		}
		return FileResult{Path: e.Path, Outcome: Filled}
	}, func(r FileResult) {
		recorded = append(recorded, string(r.Outcome)+" "+r.Path)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Each: %v, want the cancel", err)
	}
	if !slices.Equal(filled, []string{"/d/a.log", "/d/b.log"}) {
		t.Errorf("filled %v", filled)
	}
	if !slices.Equal(recorded, []string{"filled /d/a.log", "skipped /d/link"}) {
		t.Errorf("recorded %v", recorded)
	}
	if !slices.Equal(doneSeen, []int64{0, 2}) {
		t.Errorf("files_done seen while filling: %v", doneSeen)
	}
	if done, _ := progress.Files(); done != 2 {
		t.Errorf("files_done after the cancel: %d", done)
	}
}

// A run that is canceled before it starts fills nothing.
func TestEach_CanceledBeforeTheFirstFileFillsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Each(ctx, []Entry{{Path: "/d/a.log"}}, &Progress{}, func(context.Context, Entry) FileResult {
		t.Error("a file was filled")
		return FileResult{}
	}, func(FileResult) { t.Error("a file was recorded") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Each: %v", err)
	}
}

// The fraction is the files done plus the share of the current file
// read, over the files listed; the bytes are the current file's, and
// start again at each file.
func TestProgress_FractionAndBytesFollowTheCurrentFile(t *testing.T) {
	entries := []Entry{{Path: "/d/a.log"}, {Path: "/d/b.log"}}
	progress := &Progress{}
	if _, known := progress.Fraction(); known {
		t.Error("a fraction before the run started")
	}
	type seen struct {
		fraction    float64
		read, total int64
	}
	var during []seen
	err := Each(context.Background(), entries, progress, func(_ context.Context, e Entry) FileResult {
		read, total := progress.Bytes()
		if read != 0 || total != 0 {
			t.Errorf("%s starts at bytes %d/%d", e.Path, read, total)
		}
		progress.FileBytes(25, 100)
		fraction, _ := progress.Fraction()
		read, total = progress.Bytes()
		during = append(during, seen{fraction, read, total})
		return FileResult{Path: e.Path, Outcome: Filled}
	}, func(FileResult) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []seen{{0.125, 25, 100}, {0.625, 25, 100}}
	if !slices.Equal(during, want) {
		t.Errorf("progress during the files %v, want %v", during, want)
	}
	if fraction, known := progress.Fraction(); !known || fraction != 1 {
		t.Errorf("fraction at the end %v %v", fraction, known)
	}
}

// Counts puts each file in exactly one of done, skipped and failed, and
// a current file in skipped as well as current.
func TestCounts_EveryFileInOneBucket(t *testing.T) {
	var counts Counts
	for _, outcome := range []Outcome{Filled, Filled, Current, Skipped, Failed, Current} {
		counts.Add(FileResult{Outcome: outcome})
	}
	want := Counts{FilesTotal: 6, Done: 2, Skipped: 3, Current: 2, Failed: 1}
	if counts != want {
		t.Errorf("counts %+v, want %+v", counts, want)
	}
}
