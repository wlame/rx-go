package index

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filetext"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// The journal section describes the journal as it was when its text
// was opened: the header the reader read then, not a second read after
// the walk. A live journal's header moves while it is read (its state,
// its tail sequence number), and one index must not mix two of them.
func TestJournalSection_UsesTheHeaderTheReaderOpened(t *testing.T) {
	j := journalfile.Write(t, journalfile.MessageEntries("LINE 1 one", "LINE 2 two"), journalfile.Options{State: 1, FirstSeqnum: 7})
	path := filepath.Join(t.TempDir(), "system.journal")
	if err := os.WriteFile(path, j.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	text, err := filetext.Open(f, info.Size(), compression.FormatJournal)
	if err != nil {
		t.Fatalf("open the journal's text: %v", err)
	}
	defer func() { _ = text.Close() }()
	if _, err := io.Copy(io.Discard, text); err != nil {
		t.Fatalf("read the text: %v", err)
	}

	// The journal is archived after the read: its state byte changes on
	// disk, as journald changes it when it rotates a live file.
	archived := append([]byte(nil), j.Bytes...)
	archived[16] = 2
	if err := os.WriteFile(path, archived, 0o600); err != nil {
		t.Fatal(err)
	}

	section, err := journalSection(text)
	if err != nil {
		t.Fatalf("journalSection: %v", err)
	}
	if section.State != "online" || section.HeadSeqnum != 7 || section.TailSeqnum != 8 {
		t.Errorf("section %+v, want the state online of the header the text was opened with, seqnums 7 and 8", section)
	}
	if section.Entries != 2 || section.Shown != 2 {
		t.Errorf("counts %+v, want 2 entries shown", section)
	}
}
