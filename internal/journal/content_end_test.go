package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// tailObjectEnd computes, from the bytes of a journal, the offset just
// past its tail object: tail_object_offset (header offset 136) plus the
// size in that object's header (object offset 8).
func tailObjectEnd(t *testing.T, data []byte) int64 {
	t.Helper()
	tail := binary.LittleEndian.Uint64(data[136:])
	return int64(tail + binary.LittleEndian.Uint64(data[tail+8:])) //nolint:gosec // a fixture's offsets are small
}

// The content of every fixture ends where its tail object ends.
func TestContentEnd_IsTheEndOfTheTailObject(t *testing.T) {
	for _, f := range readFixtureManifest(t) {
		t.Run(f.Name, func(t *testing.T) {
			data, err := os.ReadFile(fixtureJournal(t, f.Name))
			if err != nil {
				t.Fatal(err)
			}
			end, err := ContentEnd(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("ContentEnd: %v", err)
			}
			if want := tailObjectEnd(t, data); end != want || end > int64(len(data)) {
				t.Fatalf("ContentEnd = %d, want %d (the file is %d bytes)", end, want, len(data))
			}
		})
	}
}

// Space journald preallocated after the last object is not content: a
// journal grown to 8 MiB with zeros, its arena raised to match, ends its
// content where it did, and its text is the same.
func TestContentEnd_LeavesOutPreallocatedSpace(t *testing.T) {
	path := fixtureJournal(t, "basic-zstd-compact")
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	want := tailObjectEnd(t, data)
	const preallocated = 8 << 20
	journalfixture.Preallocate(t, path, preallocated)
	grown, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	end, err := ContentEnd(bytes.NewReader(grown), int64(len(grown)))
	if err != nil {
		t.Fatalf("ContentEnd: %v", err)
	}
	if end != want {
		t.Fatalf("ContentEnd of the preallocated journal = %d, want %d", end, want)
	}
	text, err := NewTextReader(bytes.NewReader(grown), int64(len(grown)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(text)
	if err != nil {
		t.Fatal(err)
	}
	if expected := journalfixture.SetText(t, "basic"); !bytes.Equal(got, expected) {
		t.Fatalf("the preallocated journal's text is %d bytes, want the %d of basic.txt", len(got), len(expected))
	}
}

// A copy cut short, whose tail object lies beyond its end, has its
// content up to the end of the file.
func TestContentEnd_ACopyCutShortEndsAtItsEnd(t *testing.T) {
	data, err := os.ReadFile(fixtureJournal(t, "many-none-regular"))
	if err != nil {
		t.Fatal(err)
	}
	cut := data[:len(data)/2]
	end, err := ContentEnd(bytes.NewReader(cut), int64(len(cut)))
	if err != nil {
		t.Fatalf("ContentEnd: %v", err)
	}
	if end != int64(len(cut)) {
		t.Fatalf("ContentEnd of a copy cut to %d bytes = %d, want its end", len(cut), end)
	}
}

// A header rx cannot read is ContentEnd's error, as it is ReadHeader's.
func TestContentEnd_AHeaderRxCannotReadIsAnError(t *testing.T) {
	path := fixtureJournal(t, "basic-none-regular")
	journalfixture.AddUnknownIncompatibleFlag(t, path)
	data, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ContentEnd(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("ContentEnd: %v, want an error wrapping ErrUnreadable", err)
	}
}
