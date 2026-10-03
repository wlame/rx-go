package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// A build whose context is canceled stops at its next read of the text
// and returns the cancel, for every kind of file it indexes: the walk
// reads nothing past the cancel, so a status request sees the progress
// stop short of the end.
func TestBuildContext_StopsAtTheNextReadOnceCanceled(t *testing.T) {
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
	files["app.log.zst"] = nil
	seekablefile.Write(t, filepath.Join(dir, "stamped.log.zst"), seekablefile.SplitEvery(stampedProgressText(), 64*1024))
	files["stamped.log.zst"] = nil

	for name := range files {
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
