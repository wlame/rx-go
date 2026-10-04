package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// buildContextFiles writes the same text as every kind of file a build
// indexes (plain, gzip, xz, bzip2, a zstd stream, seekable zstd, and a
// seekable copy with timestamps whose text is walked beside the frame
// scan) into a new directory, and returns the directory and the names.
// A compressed kind whose tool is missing is left out.
func buildContextFiles(t *testing.T) (string, []string) {
	t.Helper()
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
	seekablefile.Write(t, filepath.Join(dir, "app.log.zst"), seekablefile.SplitEvery(text, 64*1024))
	seekablefile.Write(t, filepath.Join(dir, "stamped.log.zst"), seekablefile.SplitEvery(stampedProgressText(), 64*1024))
	names := []string{"app.log.zst", "stamped.log.zst"}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	return dir, names
}

// A build whose context is canceled stops at its next read of the text
// and returns the cancel, for every kind of file it indexes: the walk
// reads nothing past the cancel, so a status request sees the progress
// stop short of the end.
func TestBuildContext_StopsAtTheNextReadOnceCanceled(t *testing.T) {
	dir, names := buildContextFiles(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			progress := &Progress{}
			// The cancel comes after the build has opened and classified
			// the file, right before it walks the text.
			_, err := BuildContext(ctx, filepath.Join(dir, name), BuildOptions{
				StepBytes: 4096, Progress: progress, beforeWalk: cancel,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("BuildContext after a cancel: %v, want the cancel", err)
			}
			if fraction, _ := progress.Fraction(); fraction >= 1 {
				t.Errorf("the build read the whole file after the cancel (progress %v)", fraction)
			}
		})
	}
}

// A cancel that comes once the build has read part of the text stops it
// at its next read with the cancel, for every kind of file: the text read
// before the cancel does not finish the build. A plain file is read in
// 256 KiB pieces and a seekable one a 64 KiB frame at a time, so for
// them the build also stops short of the end; a compressed stream may
// arrive whole in its first read, and its cancel then lands on the read
// that would have found the end.
func TestBuildContext_StopsAtTheNextReadOnceCanceledAfterTheFirstRead(t *testing.T) {
	dir, names := buildContextFiles(t)
	stopsShortOfTheEnd := map[string]bool{"app.log": true, "app.log.zst": true, "stamped.log.zst": true}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			progress := &Progress{}
			ctx := newCanceledAfterTheFirstRead(progress)
			defer ctx.cancel()
			_, err := BuildContext(ctx, filepath.Join(dir, name), BuildOptions{StepBytes: 4096, Progress: progress})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("BuildContext canceled after its first read: %v, want the cancel", err)
			}
			fraction, _ := progress.Fraction()
			if fraction == 0 {
				t.Errorf("the cancel came before the first read")
			}
			if stopsShortOfTheEnd[name] && fraction >= 1 {
				t.Errorf("the build read the whole file after the cancel (progress %v)", fraction)
			}
		})
	}
}

// canceledAfterTheFirstRead is a context that its build's own progress
// cancels: Err reports the cancel as soon as the build has read some of
// the text, so the cancel falls between two reads of the walk at a place
// the test fixes, with no goroutine racing the build. The build checks
// Err before each read (stoppedReader, stoppedWriter), so the first read
// goes through and the next one fails.
//
// Go note: embedding context.Context gives the struct every method of
// the context it wraps, and the Err declared below takes the place of
// the embedded one. Done stays the wrapped context's channel, which is
// not nil, so the build wraps its reads in the check.
type canceledAfterTheFirstRead struct {
	context.Context
	cancel   context.CancelFunc
	progress *Progress
}

// newCanceledAfterTheFirstRead returns a context canceled once progress
// counts a byte read.
func newCanceledAfterTheFirstRead(progress *Progress) *canceledAfterTheFirstRead {
	ctx, cancel := context.WithCancel(context.Background())
	return &canceledAfterTheFirstRead{Context: ctx, cancel: cancel, progress: progress}
}

// Err cancels the context once the build has read something, then
// reports the wrapped context's error.
func (c *canceledAfterTheFirstRead) Err() error {
	if c.progress.done.Load() > 0 {
		c.cancel()
	}
	return c.Context.Err()
}

// A build whose context is never canceled builds the index Build builds.
func TestBuildContext_UncanceledBuildsAsBuildDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, progressText(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withContext, err := BuildContext(ctx, path, BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Build(path, BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if *withContext.LineCount != *plain.LineCount || len(withContext.LineIndex) != len(plain.LineIndex) {
		t.Errorf("BuildContext %d lines %d checkpoints, Build %d and %d", *withContext.LineCount,
			len(withContext.LineIndex), *plain.LineCount, len(plain.LineIndex))
	}
}
