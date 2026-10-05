package compressfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// rx compress of a journal writes its text as seekable zstd, under the
// journal's whole name with .zst added.
func TestCompress_WritesTheTextOfAJournal(t *testing.T) {
	t.Parallel()
	text := journalfixture.SetText(t, "many")
	dir := t.TempDir()
	input := filepath.Join(dir, "system.journal")
	journal, err := os.ReadFile(journalfixture.Journal(t, "many-zstd-regular"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, DefaultOutputName(input))
	if filepath.Base(output) != "system.journal.zst" {
		t.Fatalf("default output name %q, want system.journal.zst", filepath.Base(output))
	}

	result, err := Compress(context.Background(), Options{
		InputPath: input, OutputPath: output, FrameSize: 4096, Level: 3, Workers: 2,
	})
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if got := decompressedText(t, output); !bytes.Equal(got, text) {
		t.Errorf("output text differs from the journal's text (%d vs %d bytes)", len(got), len(text))
	}
	if result.InputFormat != compression.FormatJournal || result.DecompressedSize != int64(len(text)) {
		t.Errorf("input format %q, text size %d; want journal, %d", result.InputFormat, result.DecompressedSize, len(text))
	}
}

// A journal rx cannot read is refused as not text, with its reason.
func TestCompress_RefusesAJournalRxCannotRead(t *testing.T) {
	t.Parallel()
	input := journalfixture.Journal(t, "basic-none-regular")
	journalfixture.AddUnknownIncompatibleFlag(t, input)
	output := filepath.Join(filepath.Dir(input), "out.zst")
	_, err := Compress(context.Background(), Options{InputPath: input, OutputPath: output, FrameSize: 4096, Level: 3, Workers: 1})
	if !errors.Is(err, filekind.ErrNotText) {
		t.Fatalf("Compress: %v, want the not-text refusal", err)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("an output was written: %v", statErr)
	}
}
