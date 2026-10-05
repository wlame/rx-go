package index

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// damagedJournalSeqnumID is the counter id the damaged journal's header
// names.
var damagedJournalSeqnumID = [16]byte{0xab, 0xcd, 0xef, 0x01, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 0xff}

// writeDamagedJournal writes, in a new directory, a 30-entry journal
// (online, compact, zstd, its first entry numbered 1000) with two kinds of
// damage: entry 5 is zeroed, so a reader leaves it out, and the last
// ENTRY_ARRAY object of the entry list is broken, so the entries it lists
// are not reached. It returns the file's path.
func writeDamagedJournal(t *testing.T) string {
	t.Helper()
	messages := make([]string, 30)
	for i := range messages {
		messages[i] = fmt.Sprintf("LINE %d of a damaged journal", i+1)
	}
	j := journalfile.Write(t, journalfile.MessageEntries(messages...), journalfile.Options{
		Compact: true, Compression: journalfile.Zstd, State: 1, FirstSeqnum: 1000,
		SeqnumID: damagedJournalSeqnumID,
	})
	data := bytes.Clone(j.Bytes)
	j.ZeroEntry(data, 4)
	j.BreakList(data, len(j.Arrays)-1)
	path := filepath.Join(t.TempDir(), "system.journal")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readersReport reads the text of the journal at path to its end with the
// journal reader itself and returns the reader's report: what the index's
// journal section must say.
func readersReport(t *testing.T, path string) journal.Report {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	text, err := journal.NewTextReader(f, info.Size(), journal.Options{})
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer func() { _ = text.Close() }()
	if _, err := io.Copy(io.Discard, text); err != nil {
		t.Fatalf("read the text of %s: %v", path, err)
	}
	return text.Report()
}

// requireJournalCounts fails the test unless the journal section's counts
// and first damage are the report's.
func requireJournalCounts(t *testing.T, got *rxtypes.JournalInfo, want journal.Report) {
	t.Helper()
	if got == nil {
		t.Fatal("the index of a journal has no journal section")
	}
	counts := [][3]any{
		{"entries", got.Entries, want.Entries},
		{"shown", got.Shown, want.Shown},
		{"without_message", got.WithoutMessage, want.WithoutMessage},
		{"left_out", got.LeftOut, want.LeftOut},
		{"not_reached", got.NotReached, want.NotReached},
	}
	for _, c := range counts {
		if c[1] != c[2] {
			t.Errorf("journal.%s = %v, the reader's report says %v", c[0], c[1], c[2])
		}
	}
	switch {
	case want.FirstDamaged == 0 && got.FirstDamagedEntry != nil:
		t.Errorf("journal.first_damaged_entry = %d, want null", *got.FirstDamagedEntry)
	case want.FirstDamaged != 0 && (got.FirstDamagedEntry == nil || *got.FirstDamagedEntry != want.FirstDamaged):
		t.Errorf("journal.first_damaged_entry = %v, want %d", got.FirstDamagedEntry, want.FirstDamaged)
	}
	switch {
	case want.DamageReason == "" && got.DamageReason != nil:
		t.Errorf("journal.damage_reason = %q, want null", *got.DamageReason)
	case want.DamageReason != "" && (got.DamageReason == nil || *got.DamageReason != want.DamageReason):
		t.Errorf("journal.damage_reason = %v, want %q", got.DamageReason, want.DamageReason)
	}
}

// A damaged journal is indexed, and its index records the reader's report
// of the damage and the header's facts; the index stays valid until the
// file changes.
func TestBuild_RecordsTheDamageOfAJournalAndTheHeadersFacts(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := writeDamagedJournal(t)
	want := readersReport(t, path)
	if want.LeftOut == 0 || want.NotReached == 0 {
		t.Fatalf("the damaged journal is not damaged both ways: %+v", want)
	}
	if want.Entries != want.Shown+want.WithoutMessage+want.LeftOut+want.NotReached {
		t.Fatalf("the reader's counts do not add up: %+v", want)
	}

	idx, err := Build(path, BuildOptions{StepBytes: 256})
	if err != nil {
		t.Fatalf("Build of a damaged journal: %v", err)
	}
	requireJournalCounts(t, idx.Journal, want)
	got := idx.Journal
	if got.State != "online" || !got.Compact {
		t.Errorf("journal state %q, compact %v; want online, true", got.State, got.Compact)
	}
	if want := hex.EncodeToString(damagedJournalSeqnumID[:]); got.SeqnumID != want {
		t.Errorf("journal.seqnum_id = %q, want %q", got.SeqnumID, want)
	}
	if got.HeadSeqnum != 1000 || got.TailSeqnum != 1029 {
		t.Errorf("journal head and tail seqnum %d and %d, want 1000 and 1029", got.HeadSeqnum, got.TailSeqnum)
	}
	if idx.LineCount == nil || *idx.LineCount != want.Shown {
		t.Errorf("line count %v, want one line per shown entry (%d)", idx.LineCount, want.Shown)
	}

	if _, err := Save(idx); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadForSource(path)
	if err != nil || loaded == nil {
		t.Fatalf("the stored index of the unchanged journal is not used: %v", err)
	}
	requireJournalCounts(t, loaded.Journal, want)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadForSource(path); err == nil && loaded != nil {
		t.Error("the stored index is used after the journal changed")
	}
}

// The index of an undamaged journal says nothing was left out or not
// reached, with no first damaged entry and no reason, and its counts are
// journalctl's.
func TestBuild_RecordsAnUndamagedJournalAsWhole(t *testing.T) {
	for _, info := range journalfixture.Manifest(t) {
		if info.IsChainPart() || info.Set != "basic" {
			continue
		}
		t.Run(info.Name, func(t *testing.T) {
			idx, err := Build(journalfixture.Journal(t, info.Name), BuildOptions{})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			requireJournalCounts(t, idx.Journal, journal.Report{
				Entries: info.Entries, Shown: info.Shown, WithoutMessage: info.WithoutMessage,
			})
			if idx.Journal != nil && idx.Journal.Compact != info.Compact {
				t.Errorf("journal.compact = %v, want %v", idx.Journal.Compact, info.Compact)
			}
		})
	}
}

// The index of a file that is not a journal has a journal section of
// null, written as an explicit null.
func TestBuild_IndexOfAFileThatIsNotAJournalHasANullJournalSection(t *testing.T) {
	dir := t.TempDir()
	text := []byte("LINE 1 a plain line\nLINE 2 another\n")
	files := map[string][]byte{
		"plain.log":    text,
		"plain.log.gz": compressedcopy.Encode(t, compressedcopy.Gzip, text),
	}
	for name, data := range files {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			idx, err := Build(path, BuildOptions{})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if idx.Journal != nil {
				t.Fatalf("journal section %+v, want null", idx.Journal)
			}
			raw, err := json.Marshal(idx)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if value, present := doc["journal"]; !present || string(value) != "null" {
				t.Errorf("the stored index has journal %s (present %v), want an explicit null", value, present)
			}
		})
	}
}
