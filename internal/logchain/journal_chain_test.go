package logchain

// Journal chains: the rotated files of one systemd journal
// (system.journal and its system@….journal archives), ordered by their
// headers and read as the concatenation of their texts.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/filetext"
	"github.com/wlame/rx-go/internal/journal"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/journalfile"
	"github.com/wlame/rx-go/internal/testutil/journalfixture"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The counter ids (seqnum_id) of the test journals: journald numbers
// the entries of all its journals with one counter, and a new counter
// starts when its state is lost.
var (
	testCounter  = [16]byte{0xb6, 0x42, 0x3d, 0x6d, 0x2c, 0xa5, 0x43, 0xb0, 0x8b, 0x82, 0xcd, 0x46, 0xfa, 0xfc, 0xa6, 0xe4}
	otherCounter = [16]byte{0x8c, 0xe9, 0xc4, 0x33, 0x43, 0x84, 0x4c, 0xf4, 0xb3, 0x3e, 0x5c, 0x84, 0x2d, 0x34, 0x8d, 0x5c}
)

// journalBase is the time of the first entry of the test journals.
var journalBase = time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)

// journalHeaderBudget is the most a journal chain reads of a part to
// order it: its header, 272 bytes in systemd 257.
const journalHeaderBudget = journalfile.HeaderSize

// testJournal is one journal file of a test chain: its name, the counter
// that numbers its entries, the sequence number of its first entry, and
// the time of each entry.
type testJournal struct {
	name    string
	counter [16]byte
	seqnum  uint64
	times   []time.Time
}

// archiveName is the name journald gives an archive of the journal
// `name.journal` whose first entry is number seqnum of counter, written
// at the time at.
func archiveName(name string, counter [16]byte, seqnum uint64, at time.Time) string {
	return fmt.Sprintf("%s@%x-%016x-%016x.journal", name, counter[:], seqnum, at.UnixMicro())
}

// secondsFrom are n times one second apart, from start.
func secondsFrom(start time.Time, n int) []time.Time {
	times := make([]time.Time, n)
	for i := range times {
		times[i] = start.Add(time.Duration(i) * time.Second)
	}
	return times
}

// journalBytes is j as a journal file: one entry per time, numbered from
// j.seqnum, each shown as one line `… test-host demo-app[4101]: entry
// <seqnum>`. An archive is in the archived state, the active file
// online.
func journalBytes(t *testing.T, j testJournal) []byte {
	t.Helper()
	messages := make([]string, len(j.times))
	for i := range messages {
		messages[i] = fmt.Sprintf("entry %d", j.seqnum+uint64(i)) // #nosec G115 -- a small loop index
	}
	entries := journalfile.MessageEntries(messages...)
	for i, at := range j.times {
		entries[i].Realtime = uint64(at.UnixMicro()) // #nosec G115 -- times after 1970
	}
	state := uint8(2)
	if !strings.Contains(j.name, "@") {
		state = 1
	}
	return journalfile.Write(t, entries, journalfile.Options{SeqnumID: j.counter, FirstSeqnum: j.seqnum, State: state}).Bytes
}

// buildJournalChain writes journals into a new directory, in the
// chain's order, and the concatenation of their texts beside them: the
// oracle every answer is compared with.
func buildJournalChain(t *testing.T, name string, journals ...testJournal) builtChain {
	t.Helper()
	dir := t.TempDir()
	c := builtChain{dir: dir, name: name, files: map[string]chainFile{}}
	for _, j := range journals {
		path := filepath.Join(dir, j.name)
		if err := os.WriteFile(path, journalBytes(t, j), 0o600); err != nil {
			t.Fatal(err)
		}
		c.files[j.name] = chainFile{name: j.name, text: partText(t, path)}
		c.order = append(c.order, j.name)
	}
	c.concat = concatenation(t, dir, c.files, c.order)
	return c
}

// partText is the text of one file as every rx command reads it: a
// journal's rendered text, a plain file's bytes.
func partText(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	text, err := filetext.Open(f, info.Size(), filekind.FormatOf(f, info.Size()).Format)
	if err != nil {
		t.Fatalf("open the text of %s: %v", path, err)
	}
	defer func() { _ = text.Close() }()
	body, err := io.ReadAll(text)
	if err != nil {
		t.Fatalf("read the text of %s: %v", path, err)
	}
	return body
}

// chainText is the chain's text: its parts' texts in its order.
func chainText(t *testing.T, d *Description) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range d.Parts() {
		buf.Write(partText(t, p.Path))
	}
	return buf.Bytes()
}

// headerOf reads the header of the journal at path.
func headerOf(t *testing.T, path string) journal.Header {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	h, err := journal.ReadHeader(f, info.Size())
	if err != nil {
		t.Fatalf("header of %s: %v", path, err)
	}
	return h
}

// fixtureChainText is journalctl's text over every part of a fixture
// chain directory (all.txt).
func fixtureChainText(t *testing.T, chain string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(journalfixture.Root(t), chain, "all.txt")) // #nosec G304 -- a fixture
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// oracleFile writes text into a file of its own directory and returns
// its path: one file the chain's answers are compared with.
func oracleFile(t *testing.T, text []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "all.txt")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rotateActiveJournal renames dir's system.journal to the name journald
// gives it when it archives it, and returns that name.
func rotateActiveJournal(t *testing.T, dir string) string {
	t.Helper()
	active := filepath.Join(dir, "system.journal")
	h := headerOf(t, active)
	name := archiveName("system", h.SeqnumID, h.HeadSeqnum, time.UnixMicro(int64(h.HeadRealtime))) // #nosec G115 -- a fixture's time
	if err := os.Rename(active, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	return name
}

// requireState fails unless d is in state with exactly the reason codes
// codes.
func requireState(t *testing.T, d *Description, state string, codes ...string) {
	t.Helper()
	if codes == nil {
		codes = []string{}
	}
	if d.Response.State != state || !slices.Equal(reasonCodes(d), codes) {
		t.Fatalf("state %s, reasons %+v; want %s %v", d.Response.State, d.Response.Reasons, state, codes)
	}
}

// requireStartsAddUp fails unless each part's global start is 1 plus the
// line counts of the parts before it, and the chain's line count is
// lines.
func requireStartsAddUp(t *testing.T, d *Description, lines int64) {
	t.Helper()
	start := int64(1)
	for k, p := range d.Response.Parts {
		if p.GlobalStart == nil || *p.GlobalStart != start || d.Starts[k] != start || p.LineCount == nil {
			t.Fatalf("%s: global_start %v (Starts %d), line_count %v, want start %d", p.Name, p.GlobalStart, d.Starts[k], p.LineCount, start)
		}
		start += *p.LineCount
	}
	if d.Response.LineCount == nil || *d.Response.LineCount != lines || start-1 != lines {
		t.Fatalf("line_count %v (parts %d), the text has %d", d.Response.LineCount, start-1, lines)
	}
}

// The chain journald wrote with one counter is ordered by its parts'
// headers (by the sequence number of each part's first entry), its text
// is byte for byte what journalctl prints over all its parts, and its
// global line numbers and samples are those of that text, cold and with
// every part indexed.
func TestDescribe_JournalChainOfOneCounterReadsAsJournalctl(t *testing.T) {
	dir := journalfixture.ChainDir(t, "chain-one-counter")
	all := fixtureChainText(t, "chain-one-counter")
	d := describe(t, dir, "system.journal", Options{Scan: true})
	requireState(t, d, rxtypes.ChainStateReady)

	names, err := filepath.Glob(filepath.Join(dir, "*.journal"))
	if err != nil || len(names) != 3 {
		t.Fatalf("the fixture's journals: %v %v", names, err)
	}
	slices.SortFunc(names, func(a, b string) int {
		ha, hb := headerOf(t, a), headerOf(t, b)
		if ha.SeqnumID != hb.SeqnumID {
			t.Fatalf("%s and %s have two counters", a, b)
		}
		return cmp.Compare(ha.HeadSeqnum, hb.HeadSeqnum)
	})
	for i := range names {
		names[i] = filepath.Base(names[i])
	}
	if got := partNamesOf(d); !slices.Equal(got, names) {
		t.Fatalf("order %v, the headers give %v", got, names)
	}
	if text := chainText(t, d); !bytes.Equal(text, all) {
		t.Fatalf("the chain's text differs from journalctl's:\n%s\n---\n%s", text, all)
	}
	requireStartsAddUp(t, d, lineCountOf(all))

	c := builtChain{dir: dir, name: "system.journal", order: names, concat: oracleFile(t, all)}
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "1-13")},
		SamplesRequest{Lines: lines(t, "5,9"), BeforeContext: 2, AfterContext: 2},
		SamplesRequest{Lines: lines(t, "-1,6-10")},
		SamplesRequest{Timestamps: []string{"2026-10-13T12:30:49", "2026-10-13T12:30:50..2026-10-13T12:30:51"}},
	)
}

// The fixture chain whose active journal has a counter of its own (a
// second journald started without the first one's state): the archives
// of the first counter come first, the chain is ready, and its text is
// what journalctl prints, whose times do not interleave here.
func TestDescribe_JournalChainOfTwoCountersIsReady(t *testing.T) {
	dir := journalfixture.ChainDir(t, "chain-two-counters")
	d := describe(t, dir, "system.journal", Options{Scan: true})
	requireState(t, d, rxtypes.ChainStateReady)
	parts := d.Parts()
	if len(parts) != 3 || !parts[2].IsActive {
		t.Fatalf("parts %v", partNamesOf(d))
	}
	first, second, active := headerOf(t, parts[0].Path), headerOf(t, parts[1].Path), headerOf(t, parts[2].Path)
	if first.SeqnumID != second.SeqnumID || first.SeqnumID == active.SeqnumID || first.HeadSeqnum >= second.HeadSeqnum {
		t.Fatalf("headers %+v %+v %+v", first, second, active)
	}
	all := fixtureChainText(t, "chain-two-counters")
	if text := chainText(t, d); !bytes.Equal(text, all) {
		t.Fatalf("the chain's text differs from journalctl's:\n%s\n---\n%s", text, all)
	}
	requireStartsAddUp(t, d, lineCountOf(all))
}

// Shown times that go back by an hour across a rotation (a program's
// own clock) do not move a part out of its place by the headers, and
// make no time overlap: the chain is ready and answers as its parts'
// texts concatenated in that order.
func TestDescribe_JournalTimesThatGoBackAcrossARotationAreReady(t *testing.T) {
	archiveTimes := secondsFrom(journalBase, 5)
	c := buildJournalChain(t, "system.journal",
		testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1, times: archiveTimes},
		testJournal{name: "system.journal", counter: testCounter, seqnum: 6,
			times: secondsFrom(archiveTimes[4].Add(-time.Hour), 5)},
	)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	requireState(t, d, rxtypes.ChainStateReady)
	if got := partNamesOf(d); !slices.Equal(got, c.order) {
		t.Fatalf("order %v, want %v", got, c.order)
	}
	requireStartsAddUp(t, d, 10)
	requireLikeConcatenation(t, c,
		SamplesRequest{Lines: lines(t, "1-10")},
		SamplesRequest{Lines: lines(t, "5,6"), BeforeContext: 2, AfterContext: 2},
		SamplesRequest{Timestamps: []string{"2026-01-15T09:00:05", "2026-01-15T10:00:02", "2026-01-15T09:00:00..2026-01-15T10:00:01"}},
	)
}

// Parts of two counters: each counter's parts by the sequence number of
// their first entry, and the counters in the order of the head times of
// their first parts. The counters are not interleaved by time, as
// journalctl interleaves their entries.
func TestDescribe_JournalCountersFollowTheHeadTimesOfTheirFirstParts(t *testing.T) {
	c := buildJournalChain(t, "system.journal",
		testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
			times: secondsFrom(journalBase, 3)},
		testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(2*time.Hour)), counter: testCounter, seqnum: 4,
			times: secondsFrom(journalBase.Add(2*time.Hour), 3)},
		testJournal{name: archiveName("system", otherCounter, 1, journalBase.Add(time.Hour)), counter: otherCounter, seqnum: 1,
			times: secondsFrom(journalBase.Add(time.Hour), 3)},
		testJournal{name: "system.journal", counter: otherCounter, seqnum: 4, times: secondsFrom(journalBase.Add(3*time.Hour), 3)},
	)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	requireState(t, d, rxtypes.ChainStateReady)
	if got := partNamesOf(d); !slices.Equal(got, c.order) {
		t.Fatalf("order %v\nwant  %v", got, c.order)
	}
	requireLikeConcatenation(t, c, SamplesRequest{Lines: lines(t, "1-12")},
		SamplesRequest{Timestamps: []string{"2026-01-15T11:00:01", "2026-01-15T12:00:00..2026-01-15T13:00:00"}})
}

// A copy of an archive left beside it holds the same entries of the same
// counter: the chain is invalid with entries_overlap, which names both,
// and says so before any part is indexed.
func TestDescribe_ACopiedJournalArchiveOverlapsItsOriginal(t *testing.T) {
	original := testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
		times: secondsFrom(journalBase, 3)}
	c := buildJournalChain(t, "system.journal", original,
		testJournal{name: "system.journal", counter: testCounter, seqnum: 4, times: secondsFrom(journalBase.Add(time.Minute), 3)})
	copyName := archiveName("system", testCounter, 1, journalBase.Add(time.Microsecond))
	body, err := os.ReadFile(filepath.Join(c.dir, original.name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, copyName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{{Scan: true}, {}} {
		d := describe(t, c.dir, c.name, opts)
		requireState(t, d, rxtypes.ChainStateInvalid, rxtypes.ChainReasonEntriesOverlap)
		if got, want := partNamesOf(d), []string{original.name, copyName, "system.journal"}; !slices.Equal(got, want) {
			t.Fatalf("scan %v: order %v, want %v", opts.Scan, got, want)
		}
		reason := d.Response.Reasons[0]
		if !slices.Equal(reason.Parts, []string{original.name, copyName}) || reason.OverlapMs != nil ||
			!strings.Contains(reason.Message, original.name) || !strings.Contains(reason.Message, copyName) {
			t.Fatalf("scan %v: reason %+v", opts.Scan, reason)
		}
	}
}

// Archives copied from a server without their active journal are a
// chain, ready once read.
func TestDescribe_JournalArchivesWithoutTheActiveFileAreAChain(t *testing.T) {
	c := buildJournalChain(t, "system.journal",
		testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
			times: secondsFrom(journalBase, 3)},
		testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(time.Minute)), counter: testCounter, seqnum: 4,
			times: secondsFrom(journalBase.Add(time.Minute), 3)},
	)
	d := describe(t, c.dir, c.name, Options{Scan: true})
	requireState(t, d, rxtypes.ChainStateReady)
	if d.Candidate.HasActive() || !slices.Equal(partNamesOf(d), c.order) {
		t.Fatalf("parts %v, has active %v", partNamesOf(d), d.Candidate.HasActive())
	}
	requireLikeConcatenation(t, c, SamplesRequest{Lines: lines(t, "1-6")})
}

// A part of a journal chain whose bytes are not a journal (a text file
// named system.journal, or named like an archive) cannot be read as a
// journal: the chain is invalid with unreadable, named in those words.
func TestDescribe_APartOfAJournalChainThatIsNoJournalIsUnreadable(t *testing.T) {
	archive := testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
		times: secondsFrom(journalBase, 3)}
	active := testJournal{name: "system.journal", counter: testCounter, seqnum: 4, times: secondsFrom(journalBase.Add(time.Hour), 3)}
	textArchive := archiveName("system", testCounter, 9, journalBase.Add(time.Minute))
	cases := []struct {
		label    string
		journals []testJournal
		textName string
	}{
		{"a text file as the active part", []testJournal{archive}, "system.journal"},
		{"a text file named like an archive", []testJournal{archive, active}, textArchive},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			c := buildJournalChain(t, "system.journal", tc.journals...)
			writeChainFiles(t, c.dir, []chainFile{{name: tc.textName, text: timedLines(journalBase.Add(time.Minute), time.Second, 1, 3, "text")}})
			for _, opts := range []Options{{Scan: true}, {}} {
				d := describe(t, c.dir, c.name, opts)
				requireState(t, d, rxtypes.ChainStateInvalid, rxtypes.ChainReasonUnreadable)
				reason := d.Response.Reasons[0]
				prefix := tc.textName + " cannot be read as a journal: "
				if !slices.Equal(reason.Parts, []string{tc.textName}) || !strings.HasPrefix(reason.Message, prefix) ||
					len(reason.Message) == len(prefix) {
					t.Fatalf("scan %v: reason %+v", opts.Scan, reason)
				}
			}
		})
	}
}

// A member of a journal chain that is not text stays in the chain and
// makes it invalid, with or without a scan: a binary file named
// system.journal cannot be read as a journal for its signature, and an
// archive whose header has an incompatible flag rx does not know for
// the header's reason. Neither drops out to leave a chain of the other
// parts that looks ready.
func TestDescribe_AJournalChainMemberThatIsNotTextIsUnreadable(t *testing.T) {
	first := testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
		times: secondsFrom(journalBase, 3)}
	second := testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(time.Minute)), counter: testCounter, seqnum: 4,
		times: secondsFrom(journalBase.Add(time.Minute), 3)}
	active := testJournal{name: "system.journal", counter: testCounter, seqnum: 7, times: secondsFrom(journalBase.Add(2*time.Minute), 3)}

	t.Run("a binary file named system.journal", func(t *testing.T) {
		c := buildJournalChain(t, "system.journal", first, second)
		writeFiles(t, c.dir, map[string][]byte{"system.journal": binaryBytes})
		requireNoJournal(t, c.dir, "system.journal", "the file does not start with LPKSHHRH, the signature of a systemd journal")
	})
	t.Run("an archive with an incompatible flag rx does not know", func(t *testing.T) {
		c := buildJournalChain(t, "system.journal", first, second, active)
		path := filepath.Join(c.dir, second.name)
		journalfixture.AddUnknownIncompatibleFlag(t, path)
		f, err := os.Open(path) // #nosec G304 -- a test's own file
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		_, err = journal.ReadHeader(f, info.Size())
		if !errors.Is(err, journal.ErrUnreadable) || !strings.Contains(err.Error(), "bits rx does not know") {
			t.Fatalf("the archive's header: %v", err)
		}
		requireNoJournal(t, c.dir, second.name, strings.TrimPrefix(err.Error(), journal.ErrUnreadable.Error()+": "))
	})
}

// requireNoJournal fails unless the journal chain system.journal of dir
// lists name among its parts and is invalid, with and without a scan,
// for one reason: unreadable, naming that part as one that cannot be
// read as a journal because of why.
func requireNoJournal(t *testing.T, dir, name, why string) {
	t.Helper()
	if parts := partNames(resolveIn(t, dir, "system.journal")); !slices.Contains(parts, name) {
		t.Fatalf("%s is not a part: %v", name, parts)
	}
	for _, opts := range []Options{{Scan: true}, {}} {
		d := describe(t, dir, "system.journal", opts)
		requireState(t, d, rxtypes.ChainStateInvalid, rxtypes.ChainReasonUnreadable)
		reason := d.Response.Reasons[0]
		if want := name + " cannot be read as a journal: " + why; !slices.Equal(reason.Parts, []string{name}) || reason.Message != want {
			t.Fatalf("scan %v: reason %+v\nwant message %q", opts.Scan, reason, want)
		}
	}
}

// The order of a journal chain needs no index: before its parts are
// indexed the chain is pending and lists them in the order of their
// headers, and that order stays once they are read, even where the head
// times in the archives' names say otherwise.
func TestDescribe_AJournalChainIsInHeaderOrderBeforeItsPartsAreIndexed(t *testing.T) {
	// Each archive's name writes the head time of the other.
	c := buildJournalChain(t, "system.journal",
		testJournal{name: archiveName("system", testCounter, 1, journalBase.Add(2*time.Hour)), counter: testCounter, seqnum: 1,
			times: secondsFrom(journalBase, 3)},
		testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(-time.Hour)), counter: testCounter, seqnum: 4,
			times: secondsFrom(journalBase.Add(time.Hour), 3)},
		testJournal{name: "system.journal", counter: testCounter, seqnum: 7, times: secondsFrom(journalBase.Add(2*time.Hour), 3)},
	)
	if provisional := partNames(resolveIn(t, c.dir, c.name)); provisional[0] != c.order[1] {
		t.Fatalf("the names should give the other order: %v", provisional)
	}
	pending := describe(t, c.dir, c.name, Options{})
	requireState(t, pending, rxtypes.ChainStatePending)
	scanned := describe(t, c.dir, c.name, Options{Scan: true})
	requireState(t, scanned, rxtypes.ChainStateReady)
	storeIndexes(t, c.dir, c.order...)
	indexed := describe(t, c.dir, c.name, Options{})
	requireState(t, indexed, rxtypes.ChainStateReady)
	for label, d := range map[string]*Description{"pending": pending, "scanned": scanned, "indexed": indexed} {
		if got := partNamesOf(d); !slices.Equal(got, c.order) {
			t.Fatalf("%s: order %v\nwant %v", label, got, c.order)
		}
	}
	requireSameDescription(t, "scanned and indexed", scanned.Response, indexed.Response)
}

// A journal without entries (and an empty file) adds no line and keeps
// the place its name gives it: it is not ordered by its header's first
// entry, which it has none of, and takes no part in the checks.
func TestDescribe_EmptyJournalsKeepTheirPlace(t *testing.T) {
	c := buildJournalChain(t, "system.journal",
		testJournal{name: archiveName("system", testCounter, 1, journalBase), counter: testCounter, seqnum: 1,
			times: secondsFrom(journalBase, 3)},
		testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(time.Minute)), counter: testCounter, seqnum: 4},
		testJournal{name: archiveName("system", testCounter, 4, journalBase.Add(time.Hour)), counter: testCounter, seqnum: 4,
			times: secondsFrom(journalBase.Add(time.Hour), 3)},
	)
	if err := os.WriteFile(filepath.Join(c.dir, "system.journal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(c.order), "system.journal")
	for _, opts := range []Options{{Scan: true}, {}} {
		d := describe(t, c.dir, c.name, opts)
		if got := partNamesOf(d); !slices.Equal(got, want) || len(d.Response.Reasons) != 0 {
			t.Fatalf("scan %v: order %v, reasons %+v; want %v", opts.Scan, got, d.Response.Reasons, want)
		}
		if opts.Scan {
			requireState(t, d, rxtypes.ChainStateReady)
			requireStartsAddUp(t, d, 6)
		}
	}
}

// A journal renamed into an archive between the listing and the read
// (journald rotated it) is a changed part, as a renamed part of any
// chain is: the route answers 409. A new listing finds the chain as it
// is now: the archive in its place, no active file.
func TestDescribe_AJournalRenamedIntoAnArchiveIsAChangedPart(t *testing.T) {
	for _, opts := range []Options{{Scan: true}, {}} {
		dir := journalfixture.ChainDir(t, "chain-one-counter")
		c := resolveIn(t, dir, "system.journal")
		archived := rotateActiveJournal(t, dir)
		if _, err := Describe(context.Background(), c, opts); !errors.Is(err, ErrPartChanged) {
			t.Fatalf("scan %v: %v, want ErrPartChanged", opts.Scan, err)
		}
		fresh := resolveIn(t, dir, "system.journal")
		if fresh.HasActive() || len(fresh.Parts) != 3 || !slices.Contains(partNames(fresh), archived) {
			t.Fatalf("scan %v: the new listing: %v", opts.Scan, partNames(fresh))
		}
	}

	// The rotation between DescribeHandle's listing and its read of the
	// active journal: it lists again and says that the files changed
	// (409), and the chain it describes reads as before.
	dir := journalfixture.ChainDir(t, "chain-one-counter")
	open := openPart
	t.Cleanup(func() { openPart = open })
	var once sync.Once
	openPart = func(p paths.Pinned) (*os.File, error) {
		if filepath.Base(p.Path()) == "system.journal" {
			once.Do(func() { rotateActiveJournal(t, dir) })
		}
		return open(p)
	}
	d, changed, err := DescribeHandle(context.Background(), filepath.Join(dir, "system.journal"), Options{Scan: true})
	if err != nil || !changed || d.Candidate.HasActive() {
		t.Fatalf("describe: %v, changed %v, %+v", err, changed, d)
	}
	requireState(t, d, rxtypes.ChainStateReady)
	if text, all := chainText(t, d), fixtureChainText(t, "chain-one-counter"); !bytes.Equal(text, all) {
		t.Fatalf("the rotated chain's text differs:\n%s\n---\n%s", text, all)
	}
}

// Describing an indexed journal chain reads each part's header once, at
// most 272 bytes of it, besides what any chain reads; a second describe
// reads the active journal's header only (the frozen parts' are kept
// with their other data).
func TestDescribe_AJournalChainReadsEachPartsHeaderOnce(t *testing.T) {
	dir := journalfixture.ChainDir(t, "chain-one-counter")
	all, err := filepath.Glob(filepath.Join(dir, "*.journal"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(all))
	for i, path := range all {
		names[i] = filepath.Base(path)
	}
	storeIndexes(t, dir, names...)
	counts := countReads(t)
	read := readHeaderAt
	t.Cleanup(func() { readHeaderAt = read })
	var mu sync.Mutex
	var headerReads []int64
	readHeaderAt = func(r io.ReaderAt, size int64) (journal.Header, error) {
		counted := counting.NewReaderAt(r)
		h, err := read(counted, size)
		mu.Lock()
		defer mu.Unlock()
		headerReads = append(headerReads, counted.Load())
		return h, err
	}

	d := describe(t, dir, "system.journal", Options{})
	requireState(t, d, rxtypes.ChainStateReady)
	wantOpens := map[string]int{}
	for _, name := range names {
		wantOpens[name] = 1
	}
	if !maps.Equal(counts.opens, wantOpens) || len(counts.builds) != 0 || len(headerReads) != len(names) {
		t.Fatalf("opens %v, builds %v, header reads %v", counts.opens, counts.builds, headerReads)
	}
	for _, n := range headerReads {
		if n <= 0 || n > journalHeaderBudget {
			t.Fatalf("a header read of %d bytes, budget %d: %v", n, journalHeaderBudget, headerReads)
		}
	}

	counts.reset()
	headerReads = nil
	describe(t, dir, "system.journal", Options{})
	if !maps.Equal(counts.opens, map[string]int{"system.journal": 1}) || len(headerReads) != 1 {
		t.Fatalf("second describe: opens %v, header reads %v", counts.opens, headerReads)
	}
}
