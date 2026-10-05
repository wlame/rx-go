package index

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfixture"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A journal is indexed as its text: the line count and the text size are
// those of the text journalctl prints, and the index says the file is a
// compressed one in the journal format.
func TestBuild_IndexesAJournalAsItsText(t *testing.T) {
	text := journalfixture.SetText(t, "many")
	path := journalfixture.Journal(t, "many-lz4-compact")
	idx, err := Build(path, BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lines := int64(bytes.Count(text, []byte("\n")))
	if idx.LineCount == nil || *idx.LineCount != lines {
		t.Errorf("line count %v, want %d", idx.LineCount, lines)
	}
	if idx.DecompressedSizeBytes == nil || *idx.DecompressedSizeBytes != int64(len(text)) {
		t.Errorf("decompressed size %v, want %d", idx.DecompressedSizeBytes, len(text))
	}
	if idx.FileType != rxtypes.FileTypeCompressed || idx.CompressionFormat == nil || *idx.CompressionFormat != "journal" {
		t.Errorf("file type %q, compression %v; want compressed, journal", idx.FileType, idx.CompressionFormat)
	}
	if len(idx.LineIndex) < 2 {
		t.Errorf("%d checkpoints, want several at a 4 KiB step", len(idx.LineIndex))
	}
}

// A journal build reports its progress to the end, as a gzip build does.
func TestBuild_AJournalReportsProgressToTheEnd(t *testing.T) {
	for _, name := range []string{"many-none-regular", "many-zstd-compact", "basic-xz-regular"} {
		t.Run(name, func(t *testing.T) {
			progress := &Progress{}
			if _, err := Build(journalfixture.Journal(t, name), BuildOptions{StepBytes: 4096, Progress: progress}); err != nil {
				t.Fatalf("Build: %v", err)
			}
			if fraction, known := progress.Fraction(); !known || fraction != 1 {
				t.Fatalf("progress after the build = %v (known %v), want 1", fraction, known)
			}
		})
	}
}

// A canceled journal build stops at its next read of the file and
// returns the cancel, as a gzip build does: canceled before the walk, it
// reads nothing of the entries and the progress stops short of the end.
func TestBuildContext_StopsAJournalBuildAtTheNextReadOnceCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := &Progress{}
	_, err := BuildContext(ctx, journalfixture.Journal(t, "many-none-regular"), BuildOptions{
		StepBytes: 4096, Progress: progress, beforeWalk: cancel,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildContext after a cancel: %v, want the cancel", err)
	}
	if fraction, _ := progress.Fraction(); fraction >= 1 {
		t.Errorf("the build read the whole journal after the cancel (progress %v)", fraction)
	}
}

// A cancel that comes once the journal build has read part of the file
// stops it at its next read with the cancel.
func TestBuildContext_StopsAJournalBuildAfterItsFirstRead(t *testing.T) {
	progress := &Progress{}
	ctx := newCanceledAfterTheFirstRead(progress)
	defer ctx.cancel()
	_, err := BuildContext(ctx, journalfixture.Journal(t, "many-none-regular"), BuildOptions{StepBytes: 4096, Progress: progress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildContext canceled after its first read: %v, want the cancel", err)
	}
	if fraction, _ := progress.Fraction(); fraction == 0 || fraction >= 1 {
		t.Errorf("progress %v, want the cancel between the first read and the end", fraction)
	}
}

// preallocatedJournalBytes is the size the preallocated journals of these
// tests are grown to: one step of journald's preallocation.
const preallocatedJournalBytes = 8 << 20

// A live journal is preallocated: zeros after its last object, inside
// its arena. Its build reports progress against the end of its last
// object, so the progress moves from the start and a finished build
// reaches exactly 1; the index is the one of its text.
func TestBuild_APreallocatedJournalReportsProgressToTheEnd(t *testing.T) {
	path := journalfixture.Journal(t, "many-zstd-compact")
	journalfixture.Preallocate(t, path, preallocatedJournalBytes)
	progress := &Progress{}
	var beforeWalk float64
	idx, err := Build(path, BuildOptions{
		StepBytes: 4096, Progress: progress,
		beforeWalk: func() { beforeWalk, _ = progress.Fraction() },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if fraction, known := progress.Fraction(); !known || fraction != 1 {
		t.Fatalf("progress after the build = %v (known %v), want 1", fraction, known)
	}
	if beforeWalk <= 0 || beforeWalk >= 0.5 {
		t.Errorf("progress before the walk = %v, want the header's small share of the content", beforeWalk)
	}
	text := journalfixture.SetText(t, "many")
	if lines := int64(bytes.Count(text, []byte("\n"))); idx.LineCount == nil || *idx.LineCount != lines {
		t.Errorf("line count %v, want %d", idx.LineCount, lines)
	}
}

// A cancel stops a preallocated journal's build short of the end too.
func TestBuildContext_StopsAPreallocatedJournalBuildShortOfTheEnd(t *testing.T) {
	path := journalfixture.Journal(t, "many-zstd-compact")
	journalfixture.Preallocate(t, path, preallocatedJournalBytes)
	progress := &Progress{}
	ctx := newCanceledAfterTheFirstRead(progress)
	defer ctx.cancel()
	_, err := BuildContext(ctx, path, BuildOptions{StepBytes: 4096, Progress: progress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildContext canceled after its first read: %v, want the cancel", err)
	}
	if fraction, _ := progress.Fraction(); fraction == 0 || fraction >= 1 {
		t.Errorf("progress %v, want the cancel between the first read and the end", fraction)
	}
}
