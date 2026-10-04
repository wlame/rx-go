package journal

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"slices"
	"testing"
	"unicode/utf8"
)

// fuzzFieldNames are the fields a fuzzed entry can carry, in the order of
// FuzzAppendText's value arguments: bit i of the presence mask puts
// fuzzFieldNames[i] into the entry.
var fuzzFieldNames = [...]string{
	"MESSAGE", "_HOSTNAME", "SYSLOG_IDENTIFIER", "_COMM", "_PID", "SYSLOG_PID", "_SOURCE_REALTIME_TIMESTAMP",
}

// stampAtStart matches a 32-byte UTC stamp at the start of a text.
var stampAtStart = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}\+00:00`)

// fuzzSeedArgs are FuzzAppendText's arguments for one seed.
type fuzzSeedArgs struct {
	realtime uint64
	present  uint8
	values   [len(fuzzFieldNames)][]byte
}

// fuzzSeed returns FuzzAppendText's arguments for a render case: for each
// of fuzzFieldNames, the value of its last item, and the mask of the
// names the case has.
func fuzzSeed(tb testing.TB, c renderCase) fuzzSeedArgs {
	tb.Helper()
	seed := fuzzSeedArgs{realtime: c.Realtime}
	for _, f := range c.Fields {
		i := slices.Index(fuzzFieldNames[:], f.Name)
		if i < 0 {
			continue
		}
		value, err := base64.StdEncoding.DecodeString(f.ValueB64)
		if err != nil {
			tb.Fatalf("render case %s, field %s: %v", c.Name, f.Name, err)
		}
		seed.values[i] = value
		seed.present |= 1 << i
	}
	return seed
}

// fuzzEntry builds the entry the fuzz arguments describe: the fields whose
// bit is set in present, in fuzzFieldNames order, with an unrelated field
// between each two.
func fuzzEntry(realtime uint64, present uint8, values [][]byte) Entry {
	e := Entry{Realtime: realtime}
	for i, name := range fuzzFieldNames {
		if present&(1<<i) == 0 {
			continue
		}
		e.Fields = append(e.Fields, Field{Name: name, Value: values[i]}, Field{Name: "PRIORITY", Value: []byte("6")})
	}
	return e
}

// shadowed returns e with an earlier item stored before e's items for
// every field e has. Its value is printable and a valid source time, so
// it would show in the text if it won.
func shadowed(e Entry) Entry {
	out := Entry{Realtime: e.Realtime}
	for _, f := range e.Fields {
		out.Fields = append(out.Fields, Field{Name: f.Name, Value: []byte("1700000000000000")})
	}
	out.Fields = append(out.Fields, e.Fields...)
	return out
}

// printableByGoDecoder is the printable test written over Go's UTF-8
// decoder, as an independent check of utf8IsPrintable. Go's decoder
// accepts exactly the 1- to 4-byte forms systemd accepts (no overlong
// form, no surrogate, nothing above U+10FFFF; systemd's 5- and 6-byte
// forms never decode to a valid code point); systemd also refuses
// U+FDD0..U+FDEF and every code point whose low 16 bits are FFFE or
// FFFF, and the C0 and C1 controls other than tab and newline.
func printableByGoDecoder(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size == 1:
			return false
		case r < 0x20 && r != '\t' && r != '\n', r >= 0x7f && r <= 0x9f:
			return false
		case r >= 0xfdd0 && r <= 0xfdef, r&0xfffe == 0xfffe:
			return false
		}
		b = b[size:]
	}
	return true
}

func FuzzAppendText(f *testing.F) {
	for _, c := range renderCases(f) {
		s := fuzzSeed(f, c)
		v := s.values
		f.Add(s.realtime, s.present, v[0], v[1], v[2], v[3], v[4], v[5], v[6])
	}
	f.Fuzz(func(t *testing.T, realtime uint64, present uint8,
		message, hostname, identifier, comm, pid, syslogPID, source []byte) {
		values := [][]byte{message, hostname, identifier, comm, pid, syslogPID, source}
		e := fuzzEntry(realtime, present, values)
		hasMessage := present&1 != 0

		got, shown := AppendText(nil, e)
		switch {
		case !shown && len(got) != 0:
			t.Fatalf("an entry not shown appended %q", got)
		case !hasMessage && shown:
			t.Fatalf("an entry without MESSAGE shown: %q", got)
		case hasMessage && realtime > 0 && realtime < 1<<55 && !shown:
			t.Fatal("an entry with MESSAGE and a valid time not shown")
		}
		if shown {
			if !stampAtStart.Match(got) {
				t.Fatalf("the text does not start with a 32-byte stamp: %q", got[:min(len(got), 40)])
			}
			if got[len(got)-1] != '\n' {
				t.Fatalf("the last line does not end with a newline: %q", got[max(0, len(got)-40):])
			}
			if i := bytes.IndexByte(got, 0); i >= 0 {
				t.Fatalf("NUL byte at %d: %q", i, got)
			}
			if !utf8.Valid(got) {
				t.Fatalf("the text is not UTF-8: %q", got)
			}
		}

		// Appending after other bytes gives those bytes and the same
		// text, whether dst has to grow or has room.
		const earlier = "an earlier entry\n"
		for _, capacity := range []int{len(earlier), len(earlier) + len(got) + 4096} {
			dst := append(make([]byte, 0, capacity), earlier...)
			after, afterShown := AppendText(dst, e)
			if afterShown != shown || string(after) != earlier+string(got) {
				t.Fatalf("capacity %d: appended %q, want %q", capacity, after[len(earlier):], got)
			}
		}

		// Items stored before the last item of the same name never count.
		if again, againShown := AppendText(nil, shadowed(e)); againShown != shown || !bytes.Equal(again, got) {
			t.Fatalf("earlier duplicate items changed the text: %q, want %q", again, got)
		}

		if utf8IsPrintable(message) != printableByGoDecoder(message) {
			t.Fatalf("utf8IsPrintable(%q) = %t, Go's decoder says %t",
				message, utf8IsPrintable(message), printableByGoDecoder(message))
		}
	})
}
