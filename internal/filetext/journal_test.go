package filetext

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// openFixture opens the journal fixture name and returns the file and
// its size; the file is closed when the test ends.
func openFixture(t *testing.T, name string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(journalfixture.Journal(t, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, info.Size()
}

// The text of every journal fixture is the text journalctl printed for
// it, and once it is read the report counts its entries as journalctl
// did, with nothing damaged.
func TestOpen_AJournalGivesTheTextJournalctlPrints(t *testing.T) {
	for _, info := range journalfixture.Manifest(t) {
		if info.IsChainPart() {
			continue
		}
		t.Run(info.Name, func(t *testing.T) {
			f, size := openFixture(t, info.Name)
			text, err := Open(f, size, compression.FormatJournal)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = text.Close() }()
			got, err := io.ReadAll(text)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if want := journalfixture.SetText(t, info.Set); !bytes.Equal(got, want) {
				t.Fatalf("the text is %d bytes, journalctl printed %d", len(got), len(want))
			}
			report, ok := text.JournalReport()
			if !ok {
				t.Fatal("a journal has no report")
			}
			if report.Entries != info.Entries || report.Shown != info.Shown ||
				report.WithoutMessage != info.WithoutMessage || report.Damaged() {
				t.Errorf("report %+v; want %d entries, %d shown, %d without a message, none damaged",
					report, info.Entries, info.Shown, info.WithoutMessage)
			}
		})
	}
}

// Closing a journal's text leaves the file open for its owner.
func TestOpen_ClosingAJournalTextLeavesTheFileOpen(t *testing.T) {
	f, size := openFixture(t, "basic-xz-regular")
	text, err := Open(f, size, compression.FormatJournal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, text); err != nil {
		t.Fatal(err)
	}
	if err := text.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 8), 0); err != nil {
		t.Fatalf("the journal after its text's Close: %v, want it still open", err)
	}
}

// A journal whose header rx cannot read is an error from Open that says
// so, and no text.
func TestOpen_AJournalRxCannotReadIsAnError(t *testing.T) {
	path := journalfixture.Journal(t, "basic-none-regular")
	journalfixture.AddUnknownIncompatibleFlag(t, path)
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if text, err := Open(f, info.Size(), compression.FormatJournal); !errors.Is(err, journal.ErrUnreadable) {
		if text != nil {
			_ = text.Close()
		}
		t.Fatalf("Open: %v, want an error wrapping journal.ErrUnreadable", err)
	}
}
