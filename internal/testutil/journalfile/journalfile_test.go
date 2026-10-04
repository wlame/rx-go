package journalfile

import (
	"bytes"
	"fmt"
	"testing"
)

// numberedEntries returns n entries whose message holds their number,
// with a value of valueSize bytes that every entry shares.
func numberedEntries(n, valueSize int) []Entry {
	shared := bytes.Repeat([]byte("v"), valueSize)
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{
			Realtime:  1_760_000_000_000_000 + uint64(i), // #nosec G115 -- a small loop index
			Monotonic: 1_000 + uint64(i),                 // #nosec G115 -- a small loop index
			Payloads: [][]byte{
				Payload("MESSAGE", fmt.Appendf(nil, "entry %d", i)),
				Payload("SHARED", shared),
			},
		}
	}
	return entries
}

func TestWrite_ListsEveryEntryInOrderThroughGrowingArrays(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			j := Write(t, numberedEntries(50, 10), Options{Compact: compact})
			if got := le.Uint64(j.Bytes[OffsetNEntries:]); got != 50 {
				t.Fatalf("n_entries = %d, want 50", got)
			}
			// Capacities 4, 8, 26, 78: journald's growth.
			var capacities []int
			listed := 0
			for k, a := range j.Arrays {
				capacities = append(capacities, a.Capacity)
				if a.FirstEntry != listed {
					t.Errorf("array %d starts at entry %d, want %d", k, a.FirstEntry, listed)
				}
				listed += a.Count
				next := le.Uint64(j.Bytes[a.Offset+OffsetNextEntryArray:])
				if k+1 < len(j.Arrays) && next != j.Arrays[k+1].Offset {
					t.Errorf("array %d links to %d, want %d", k, next, j.Arrays[k+1].Offset)
				}
				if k+1 == len(j.Arrays) && next != 0 {
					t.Errorf("the last array links to %d, want 0", next)
				}
			}
			if fmt.Sprint(capacities) != "[4 8 26 78]" || listed != 50 {
				t.Errorf("capacities %v listing %d entries, want [4 8 26 78] listing 50", capacities, listed)
			}
			if uint64(len(j.Bytes))%8 != 0 || le.Uint64(j.Bytes[offsetArenaSize:]) != uint64(len(j.Bytes))-HeaderSize {
				t.Errorf("file of %d bytes with arena_size %d", len(j.Bytes), le.Uint64(j.Bytes[offsetArenaSize:]))
			}
		})
	}
}

func TestWrite_SharesEqualPayloadsAndCompressesOnlyLargeOnes(t *testing.T) {
	for _, c := range Compressions {
		t.Run(string(c), func(t *testing.T) {
			j := Write(t, numberedEntries(3, 4096), Options{Compression: c})
			// Three messages and one shared value.
			if len(j.Data) != 4 {
				t.Fatalf("%d DATA objects, want 4", len(j.Data))
			}
			for _, d := range j.Data {
				large := len(d.Payload) >= defaultCompressThreshold
				want := None
				if large {
					want = c
				}
				if d.StoredAs != want {
					t.Errorf("a %d-byte payload is stored as %s, want %s", len(d.Payload), d.StoredAs, want)
				}
				if got := j.Bytes[d.Offset+1]; got != objectFlags[d.StoredAs] {
					t.Errorf("object flags %d, want %d", got, objectFlags[d.StoredAs])
				}
			}
			// The first entry lists its message first; the later ones list
			// the shared value first, because the entry lists its items by
			// offset and the shared value was written before their
			// messages.
			for i := range j.Entries {
				got := j.EntryPayloads(i)
				message := fmt.Appendf(nil, "MESSAGE=entry %d", i)
				messageAt := 1
				if i == 0 {
					messageAt = 0
				}
				if len(got) != 2 || !bytes.Equal(got[messageAt], message) {
					t.Errorf("entry %d lists %q, want %q at %d", i, got, message, messageAt)
				}
			}
		})
	}
}
