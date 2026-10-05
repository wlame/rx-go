package filekind

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
)

// journalBytes returns the bytes of the journal fixture name.
func journalBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(journalfixture.Journal(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// kindOfFile writes body as dir/name and returns what Of and
// OfForReading decide for it; the test fails when the two differ.
func kindOfFile(t *testing.T, name string, body []byte) Kind {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	listing := Of(f, int64(len(body)))
	reading := OfForReading(f, int64(len(body)))
	if listing.Format != reading.Format || listing.NotText != reading.NotText {
		t.Fatalf("%s: Of gives %q (%q), OfForReading %q (%q)", name,
			listing.Format, listing.NotText, reading.Format, reading.NotText)
	}
	return listing
}

// Names that lie: the bytes decide. A journal named app.log is a
// journal, a text file named system.journal is text, and a gzip copy of
// a journal is a gzip file whose text is not text.
func TestOf_AJournalIsKnownByItsBytesWhateverItsName(t *testing.T) {
	journal := journalBytes(t, "basic-zstd-compact")
	cases := []struct {
		name       string
		body       []byte
		format     compression.Format
		notText    string
		isJournal  bool
		compressed bool
	}{
		{"app.log", journal, compression.FormatJournal, "", true, true},
		{"system.journal", []byte(logText), compression.FormatNone, "", false, false},
		{
			"system@0123456789abcdef0123456789abcdef-0000000000000001-00065db7fbffd8e8.journal.gz",
			gzipOf(t, journal), compression.FormatGzip,
			NotTextPrefix + ": a NUL byte in the first 8 KiB of its decompressed text", false, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := kindOfFile(t, tc.name, tc.body)
			if kind.Format != tc.format || kind.NotText != tc.notText {
				t.Fatalf("format %q, not text %q; want %q, %q", kind.Format, kind.NotText, tc.format, tc.notText)
			}
			if kind.IsJournal() != tc.isJournal || kind.IsCompressed() != tc.compressed || kind.IsSeekable() {
				t.Errorf("journal %v, compressed %v, seekable %v", kind.IsJournal(), kind.IsCompressed(), kind.IsSeekable())
			}
		})
	}
}

// A journal reports "journal" as its compression, is read through a
// decoder and is not seekable.
func TestKind_AJournalIsACompressedFormatNamedJournal(t *testing.T) {
	kind := Kind{Format: compression.FormatJournal}
	if kind.CompressionName() != "journal" || !kind.IsJournal() || !kind.IsCompressed() || kind.IsSeekable() {
		t.Fatalf("name %q, journal %v, compressed %v, seekable %v",
			kind.CompressionName(), kind.IsJournal(), kind.IsCompressed(), kind.IsSeekable())
	}
	for _, other := range []compression.Format{compression.FormatNone, compression.FormatGzip, compression.FormatSeekableZstd} {
		if (Kind{Format: other}).IsJournal() {
			t.Errorf("%q is taken for a journal", other)
		}
	}
}

// A journal whose header rx cannot read is not text, with the journal's
// reason: every command refuses it with that reason.
func TestOf_AJournalRxCannotReadIsNotTextWithItsReason(t *testing.T) {
	unknownFlag := journalfixture.Journal(t, "basic-none-regular")
	journalfixture.AddUnknownIncompatibleFlag(t, unknownFlag)
	flagged, err := os.ReadFile(unknownFlag) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
		why  string
	}{
		{"an unknown incompatible flag", flagged, "incompatible flags"},
		{"a header cut short", flagged[:100], "shorter than the smallest journal header"},
	}
	const prefix = NotTextPrefix + ": a systemd journal rx cannot read: "
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := kindOfFile(t, "system.journal", tc.body)
			if kind.Format != compression.FormatJournal {
				t.Fatalf("format %q, want journal", kind.Format)
			}
			if !strings.HasPrefix(kind.NotText, prefix) || !strings.Contains(kind.NotText, tc.why) {
				t.Fatalf("not text %q, want %q followed by why (%s)", kind.NotText, prefix, tc.why)
			}
			if !errors.Is(kind.Err(), ErrNotText) {
				t.Errorf("Err() = %v, want it to wrap ErrNotText", kind.Err())
			}
		})
	}
}

// A listing decides a journal is text from its header alone: it reads
// the signature and the header, and renders no entry.
func TestOf_AJournalInAListingCostsItsHeader(t *testing.T) {
	path := journalfixture.Journal(t, "many-none-regular")
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	const headerBudget = 512
	for name, decide := range map[string]func(r *counting.ReaderAt, size int64) Kind{
		"Of":           func(r *counting.ReaderAt, size int64) Kind { return Of(r, size) },
		"OfForReading": func(r *counting.ReaderAt, size int64) Kind { return OfForReading(r, size) },
	} {
		counter := counting.NewReaderAt(f)
		kind := decide(counter, info.Size())
		if !kind.IsJournal() || !kind.IsText() {
			t.Fatalf("%s: format %q, not text %q", name, kind.Format, kind.NotText)
		}
		if read := counter.Load(); read > headerBudget {
			t.Errorf("%s read %d bytes of a %d-byte journal, want at most %d", name, read, info.Size(), headerBudget)
		}
	}
}

// The head of a journal's text is the start of the text journalctl
// prints for it, and a head larger than the text is the whole text.
func TestReadTextHead_AJournalGivesTheStartOfItsText(t *testing.T) {
	want := journalfixture.SetText(t, "basic")
	path := journalfixture.Journal(t, "basic-lz4-compact")
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	kind := OfForReading(f, info.Size())
	for _, limit := range []int{100, len(want), len(want) + 4096} {
		head, err := ReadTextHead(f, info.Size(), kind, limit)
		if err != nil {
			t.Fatalf("ReadTextHead(%d): %v", limit, err)
		}
		if !bytes.Equal(head, want[:min(limit, len(want))]) {
			t.Errorf("ReadTextHead(%d) gives %q, want the start of the text", limit, head)
		}
	}
}
