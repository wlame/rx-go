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
