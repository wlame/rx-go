package journal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/journalfile"
)

// smallJournal returns the bytes of a valid journal of three entries.
func smallJournal(tb testing.TB) []byte {
	tb.Helper()
	entries := make([]journalfile.Entry, 3)
	for i := range entries {
		entries[i] = journalfile.Entry{
			Realtime: caseRealtime + uint64(i), // #nosec G115 -- a small loop index
			Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("small"))},
		}
	}
	return journalfile.Write(tb, entries, journalfile.Options{}).Bytes
}

// withUint64 returns a copy of data with v written at offset.
func withUint64(data []byte, offset int, v uint64) []byte {
	out := bytes.Clone(data)
	binary.LittleEndian.PutUint64(out[offset:], v)
	return out
}

func TestReadHeader_ReadsTheFieldsOfEveryFixture(t *testing.T) {
	for _, f := range readFixtureManifest(t) {
		t.Run(f.Name, func(t *testing.T) {
			data, err := os.ReadFile(fixtureJournal(t, f.Name))
			if err != nil {
				t.Fatal(err)
			}
			h, err := ReadHeader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			if h.Compact != f.Compact {
				t.Errorf("Compact = %v, want %v", h.Compact, f.Compact)
			}
			if h.Compact != (h.IncompatibleFlags&(1<<4) != 0) {
				t.Errorf("Compact = %v with incompatible flags %#x", h.Compact, h.IncompatibleFlags)
			}
			if h.NEntries != uint64(f.Entries) { // #nosec G115 -- a count from the manifest
				t.Errorf("NEntries = %d, want %d", h.NEntries, f.Entries)
			}
			if h.HeaderSize < 208 || h.HeaderSize+h.ArenaSize != uint64(len(data)) {
				t.Errorf("header %d + arena %d bytes in a file of %d", h.HeaderSize, h.ArenaSize, len(data))
			}
			if h.State > StateArchived || h.SeqnumID == ([16]byte{}) {
				t.Errorf("state %d, seqnum id %x", h.State, h.SeqnumID)
			}
			// Sequence numbers rise from entry to entry; times need not
			// (the stamps set steps back in time on purpose).
			if h.TailSeqnum-h.HeadSeqnum+1 != h.NEntries || h.HeadRealtime == 0 || h.TailRealtime == 0 {
				t.Errorf("seqnums %d..%d and times %d..%d for %d entries", h.HeadSeqnum, h.TailSeqnum, h.HeadRealtime, h.TailRealtime, h.NEntries)
			}
			if h.EntryArrayOffset < h.HeaderSize || h.TailObjectOffset < h.EntryArrayOffset {
				t.Errorf("entry list at %d, tail object at %d", h.EntryArrayOffset, h.TailObjectOffset)
			}
		})
	}
}

func TestReadHeader_ReadsTheFieldsAWriterSet(t *testing.T) {
	entries := []journalfile.Entry{
		{Realtime: 1_000_000, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("one"))}},
		{Realtime: 3_000_000, Payloads: [][]byte{journalfile.Payload("MESSAGE", []byte("two"))}},
	}
	seqnumID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	j := journalfile.Write(t, entries, journalfile.Options{
		Compact: true, Compression: journalfile.Zstd, State: 1, FirstSeqnum: 70, SeqnumID: seqnumID,
	})
	h, err := ReadHeader(bytes.NewReader(j.Bytes), int64(len(j.Bytes)))
	if err != nil {
		t.Fatal(err)
	}
	want := Header{
		State:             StateOnline,
		IncompatibleFlags: 1<<3 | 1<<4,
		Compact:           true,
		HeaderSize:        journalfile.HeaderSize,
		ArenaSize:         uint64(len(j.Bytes)) - journalfile.HeaderSize,
		TailObjectOffset:  j.Entries[1].Offset, // the second entry is written after the array
		EntryArrayOffset:  j.Arrays[0].Offset,
		NEntries:          2,
		SeqnumID:          seqnumID,
		HeadSeqnum:        70,
		TailSeqnum:        71,
		HeadRealtime:      1_000_000,
		TailRealtime:      3_000_000,
	}
	if h != want {
		t.Errorf("ReadHeader = %+v\nwant          %+v", h, want)
	}
}

func TestReadHeader_RefusesOnlyAHeaderItCannotRead(t *testing.T) {
	good := smallJournal(t)
	stateAt, flagsAt := 16, 12
	withState := func(state byte) []byte {
		out := bytes.Clone(good)
		out[stateAt] = state
		return out
	}
	withFlags := func(flags uint32) []byte {
		out := bytes.Clone(good)
		binary.LittleEndian.PutUint32(out[flagsAt:], flags)
		return out
	}
	rows := []struct {
		name   string
		data   []byte
		reason string
	}{
		{"a file shorter than the smallest header", good[:200], "shorter than the smallest journal header"},
		{"header_size below 208", withUint64(good, 88, 200), "header_size 200 is below 208"},
		{"header_size beyond the file", withUint64(good, 88, uint64(len(good))+8), "beyond the end of the file"},
		{"state 3", withState(3), "state 3"},
		{"state 255", withState(255), "state 255"},
		{"an unknown incompatible bit", withFlags(1 << 5), "0x20"},
		{"a known and an unknown bit", withFlags(1<<4 | 1<<31), "0x80000000"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := ReadHeader(bytes.NewReader(row.data), int64(len(row.data)))
			if !errors.Is(err, ErrUnreadable) {
				t.Fatalf("ReadHeader error = %v, want one wrapping ErrUnreadable", err)
			}
			if !strings.HasPrefix(err.Error(), ErrUnreadable.Error()+": ") || !strings.Contains(err.Error(), row.reason) {
				t.Errorf("error %q, want %q followed by a reason holding %q", err, ErrUnreadable.Error()+": ", row.reason)
			}
			if _, err := NewTextReader(bytes.NewReader(row.data), int64(len(row.data)), Options{}); !errors.Is(err, ErrUnreadable) {
				t.Errorf("NewTextReader error = %v, want one wrapping ErrUnreadable", err)
			}
		})
	}
}

func TestReadHeader_AcceptsAHeaderWhoseOffsetsPointBeyondTheFile(t *testing.T) {
	good := smallJournal(t)
	rows := map[string][]byte{
		"arena beyond the file":         withUint64(good, 96, 1<<40),
		"arena that overflows":          withUint64(good, 96, ^uint64(0)),
		"tail object beyond the file":   withUint64(good, 136, 1<<40),
		"entry list beyond the file":    withUint64(good, 176, 1<<40),
		"truncated after the header":    good[:journalfile.HeaderSize],
		"every known incompatible flag": func() []byte { out := bytes.Clone(good); out[12] = 0x1f; return out }(),
	}
	for name, data := range rows {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadHeader(bytes.NewReader(data), int64(len(data))); err != nil {
				t.Errorf("ReadHeader: %v", err)
			}
		})
	}
}

func TestReadHeader_FileWithoutTheSignatureIsNotAnUnreadableJournal(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":       nil,
		"text":        []byte(strings.Repeat("not a journal\n", 40)),
		"short magic": []byte("LPKS"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadHeader(bytes.NewReader(data), int64(len(data)))
			if err == nil || errors.Is(err, ErrUnreadable) {
				t.Errorf("ReadHeader error = %v, want an error that does not wrap ErrUnreadable", err)
			}
		})
	}
}

func TestReadHeader_ReturnsAnIOErrorWrapped(t *testing.T) {
	good := smallJournal(t)
	failure := errors.New("disk on fire")
	_, err := ReadHeader(failingReaderAt{data: good, from: 0, err: failure}, int64(len(good)))
	if !errors.Is(err, failure) || errors.Is(err, ErrUnreadable) {
		t.Errorf("ReadHeader error = %v, want one wrapping %v and not ErrUnreadable", err, failure)
	}
}

func TestState_StringNamesEachState(t *testing.T) {
	for state, want := range map[State]string{
		StateOffline: "offline", StateOnline: "online", StateArchived: "archived", State(7): "state 7",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", uint8(state), got, want)
		}
	}
}
