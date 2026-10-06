package trace

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// damagedJournal is a damaged journal a test wrote: its path, the text a
// reader gives of it and the reader's report.
type damagedJournal struct {
	path   string
	text   []byte
	report journal.Report
}

// writeDamagedJournal writes, into dir as name, a 12-entry journal whose
// messages are messages (12 of them): entry 3 is zeroed, so a reader
// leaves it out, and the entry list breaks at its second and last
// ENTRY_ARRAY object, so entries 5 to 12 are not reached. The text holds
// entries 1, 2 and 4.
func writeDamagedJournal(t *testing.T, dir, name string, messages []string) damagedJournal {
	t.Helper()
	j := journalfile.Write(t, journalfile.MessageEntries(messages...), journalfile.Options{Compression: journalfile.LZ4})
	data := bytes.Clone(j.Bytes)
	j.ZeroEntry(data, 2)
	j.BreakList(data, len(j.Arrays)-1)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.NewTextReader(bytes.NewReader(data), int64(len(data)), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	text, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	out := damagedJournal{path: path, text: text, report: reader.Report()}
	if out.report.LeftOut == 0 || out.report.NotReached == 0 {
		t.Fatalf("the journal is not damaged both ways: %+v", out.report)
	}
	return out
}

// skipReason is the reason the answer gives for the damaged journal.
func (d damagedJournal) skipReason() string {
	return fmt.Sprintf("not searched in full: a damaged journal; %d entries left out, %d not reached; the other entries are searched",
		d.report.LeftOut, d.report.NotReached)
}

// linesHolding returns, for each line of text that holds word, its
// 1-based line number by its byte offset.
func linesHolding(text []byte, word string) map[int64]int {
	out := map[int64]int{}
	offset := int64(0)
	for i, line := range strings.SplitAfter(string(text), "\n") {
		if strings.Contains(line, word) {
			out[offset] = i + 1
		}
		offset += int64(len(line))
	}
	return out
}

// needleMessages are twelve messages, NEEDLE in the first, the third
// (which the damage leaves out), the fourth and the sixth (which it does
// not reach).
func needleMessages() []string {
	messages := make([]string, 12)
	for i := range messages {
		messages[i] = fmt.Sprintf("LINE %d hay", i+1)
	}
	for _, i := range []int{0, 2, 3, 5} {
		messages[i] = fmt.Sprintf("LINE %d NEEDLE", i+1)
	}
	return messages
}

// requireListedOnce fails the test unless path is in skipped_files
// exactly once, with reason in skip_reasons.
func requireListedOnce(t *testing.T, resp *rxtypes.TraceResponse, path, reason string) {
	t.Helper()
	count := 0
	for _, skipped := range resp.SkippedFiles {
		if skipped == path {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("skipped_files lists %s %d times, want once: %v", filepath.Base(path), count, resp.SkippedFiles)
	}
	for _, item := range resp.SkipReasons {
		if item.Path == path && item.Reason != reason {
			t.Errorf("skip reason of %s:\n %q\nwant\n %q", filepath.Base(path), item.Reason, reason)
		}
	}
}

// A search of a damaged journal keeps the matches of the entries it
// reads, with their line numbers in the text, lists the journal once as
// not searched in full with the counts of the damage, and logs it.
func TestTrace_ListsADamagedJournalOnceAndKeepsItsMatches(t *testing.T) {
	requireRipgrep(t)
	log := captureDefaultLog(t)
	damaged := writeDamagedJournal(t, t.TempDir(), "system.journal", needleMessages())

	resp := traceOnce(t, damaged.path, []string{"NEEDLE"}, Options{NoCache: true})

	requireListedOnce(t, resp, damaged.path, damaged.skipReason())
	requireRipgrepLines(t, resp, linesHolding(damaged.text, "NEEDLE"))
	for _, want := range []string{
		"journal_damaged", "path=" + damaged.path,
		fmt.Sprintf("left_out=%d", damaged.report.LeftOut),
		fmt.Sprintf("not_reached=%d", damaged.report.NotReached),
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log.String())
		}
	}
}

// An undamaged journal is searched in full: it is not listed.
func TestTrace_DoesNotListAnUndamagedJournal(t *testing.T) {
	requireRipgrep(t)
	j := journalfile.Write(t, journalfile.MessageEntries(needleMessages()...), journalfile.Options{})
	path := filepath.Join(t.TempDir(), "system.journal")
	if err := os.WriteFile(path, j.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	resp := traceOnce(t, path, []string{"NEEDLE"}, Options{NoCache: true})
	if len(resp.SkippedFiles) != 0 || len(resp.Matches) != 4 {
		t.Errorf("skipped %v, %d matches; want none skipped and 4 matches", resp.SkipReasons, len(resp.Matches))
	}
}

// A capped search of a directory holding a damaged journal, a gzip file
// and plain files keeps the cap across the files, numbers the journal's
// matches exactly, and lists the journal once. The walk takes the files
// in name order: the journal's two matches, then the gzip file's first,
// and the cap is reached before the plain files.
func TestTrace_CappedSearchOfADirectoryWithADamagedJournal(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	damaged := writeDamagedJournal(t, dir, "a.journal", needleMessages())
	gzText := []byte("LINE 1 NEEDLE gz\nLINE 2 hay\nLINE 3 NEEDLE gz\n")
	gz := filepath.Join(dir, "b.log.gz")
	if err := os.WriteFile(gz, compressedcopy.Encode(t, compressedcopy.Gzip, gzText), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c.log", "d.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("LINE 1 NEEDLE plain\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	maxResults := 3

	resp := traceOnce(t, dir, []string{"NEEDLE"}, Options{NoCache: true, MaxResults: &maxResults})

	if len(resp.Matches) != maxResults {
		t.Fatalf("%d matches, want the cap of %d: %+v", len(resp.Matches), maxResults, resp.Matches)
	}
	want := linesHolding(damaged.text, "NEEDLE")
	byFile := map[string]int{}
	for _, m := range resp.Matches {
		path := resp.Files[m.File]
		byFile[filepath.Base(path)]++
		if path != damaged.path {
			continue
		}
		if line, ok := want[m.Offset]; !ok || m.AbsoluteLineNumber != line {
			t.Errorf("journal match at offset %d: line %d, want %d (a line holding NEEDLE: %v)",
				m.Offset, m.AbsoluteLineNumber, line, ok)
		}
	}
	if byFile["a.journal"] != len(want) || byFile["b.log.gz"] != maxResults-len(want) {
		t.Errorf("matches by file %v, want the journal's %d and the gzip file's %d", byFile, len(want), maxResults-len(want))
	}
	requireListedOnce(t, resp, damaged.path, damaged.skipReason())
	if len(resp.SkippedFiles) != 1 {
		t.Errorf("skipped %v, want the journal alone", resp.SkipReasons)
	}
}
