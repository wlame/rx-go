package journal

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// caseRealtime is the ENTRY realtime the hand-written entries below use,
// and caseStamp its text. standardPrefix is the first-line prefix of an
// entry made by standardEntry, and standardIndent its length: the indent
// of every continuation line of such an entry.
const (
	caseRealtime   = 1760000000000007
	caseStamp      = "2025-10-09T08:53:20.000007+00:00"
	standardPrefix = caseStamp + " testhost demo[100]: "
	standardIndent = len(standardPrefix)
)

// entryOf returns an entry at caseRealtime whose fields are the given
// name, value pairs, in that stored order.
func entryOf(nameValues ...string) Entry {
	if len(nameValues)%2 != 0 {
		panic("entryOf takes name, value pairs")
	}
	e := Entry{Realtime: caseRealtime}
	for i := 0; i < len(nameValues); i += 2 {
		e.Fields = append(e.Fields, Field{Name: nameValues[i], Value: []byte(nameValues[i+1])})
	}
	return e
}

// standardEntry returns an entry with the hostname testhost, the
// identifier demo, the pid 100, a priority and the given message.
func standardEntry(message string) Entry {
	return entryOf("_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", "demo", "_PID", "100",
		"PRIORITY", "6", "MESSAGE", message)
}

// renderString returns AppendText's text for e, appended to nothing, as a
// string.
func renderString(e Entry) (string, bool) {
	out, shown := AppendText(nil, e)
	return string(out), shown
}

// indented returns the continuation line for text: indent spaces, the
// text and a newline.
func indented(indent int, text string) string {
	return strings.Repeat(" ", indent) + text + "\n"
}

// firstDifference describes where got and want first differ, with a few
// bytes of each around that place, so that a mismatch in a 2 MiB text
// stays readable.
func firstDifference(got, want []byte) string {
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	from := max(0, at-24)
	return fmt.Sprintf("first difference at byte %d (got %d bytes, want %d): got %q, want %q",
		at, len(got), len(want), got[from:min(len(got), at+24)], want[from:min(len(want), at+24)])
}

// entryFromCase turns a render case into the Entry the renderer takes.
func entryFromCase(tb testing.TB, c renderCase) Entry {
	tb.Helper()
	e := Entry{Realtime: c.Realtime, Fields: make([]Field, 0, len(c.Fields))}
	for _, f := range c.Fields {
		value, err := base64.StdEncoding.DecodeString(f.ValueB64)
		if err != nil {
			tb.Fatalf("render case %s, field %s: %v", c.Name, f.Name, err)
		}
		e.Fields = append(e.Fields, Field{Name: f.Name, Value: value})
	}
	return e
}

// renderRow is one row of the focused tables below: an entry and the text
// journalctl prints for it.
type renderRow struct {
	name  string
	entry Entry
	want  string
}

// checkRows renders each row's entry and compares it with the row's text.
func checkRows(t *testing.T, rows []renderRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, shown := renderString(row.entry)
			if !shown {
				t.Fatalf("entry not shown, want %q", row.want)
			}
			if got != row.want {
				t.Errorf("got  %q\nwant %q", got, row.want)
			}
		})
	}
}

func TestAppendText_MatchesJournalctlOnEveryRenderCase(t *testing.T) {
	const (
		wantEntries = 174
		wantShown   = 172
	)
	cases := renderCases(t)
	shown := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, isShown := AppendText(nil, entryFromCase(t, c))
			if isShown {
				shown++
			}
			if isShown != (c.Expected != "") {
				t.Errorf("shown = %t, journalctl printed %d bytes", isShown, len(c.Expected))
			}
			if !bytes.Equal(got, []byte(c.Expected)) {
				t.Error(firstDifference(got, []byte(c.Expected)))
			}
		})
	}
	if len(cases) != wantEntries || shown != wantShown {
		t.Errorf("%d entries with %d shown, want %d with %d shown", len(cases), shown, wantEntries, wantShown)
	}
}

func TestAppendText_EntryWithoutMessageIsNotShown(t *testing.T) {
	const earlier = "an earlier entry\n"
	for name, e := range map[string]Entry{
		"no MESSAGE":                entryOf("_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", "demo", "_PID", "100"),
		"MESSAGE_ID and MESSAGEX":   entryOf("MESSAGE_ID", "0123456789abcdef0123456789abcdef", "MESSAGEX", "no"),
		"lower-case message":        entryOf("message", "lower case"),
		"name with a leading space": entryOf(" MESSAGE", "leading space"),
		"no field at all":           {Realtime: caseRealtime},
	} {
		t.Run(name, func(t *testing.T) {
			dst := []byte(earlier)
			got, shown := AppendText(dst, e)
			if shown {
				t.Error("entry shown")
			}
			if string(got) != earlier {
				t.Errorf("dst became %q, want it unchanged", got)
			}
		})
	}
}

func TestAppendText_DuplicateFieldLastStoredWins(t *testing.T) {
	checkRows(t, []renderRow{
		{"message", standardEntry("first").withField("MESSAGE", "second"), standardPrefix + "second\n"},
		{"empty message last", standardEntry("first").withField("MESSAGE", ""), standardPrefix + "\n"},
		{"hostname", entryOf("_HOSTNAME", "first", "_HOSTNAME", "second", "SYSLOG_IDENTIFIER", "demo", "MESSAGE", "m"),
			caseStamp + " second demo: m\n"},
		{"unprintable hostname last drops the hostname",
			entryOf("_HOSTNAME", "good", "_HOSTNAME", "b\x1bad", "SYSLOG_IDENTIFIER", "demo", "MESSAGE", "m"),
			caseStamp + " demo: m\n"},
		{"identifier", entryOf("SYSLOG_IDENTIFIER", "first", "SYSLOG_IDENTIFIER", "second", "MESSAGE", "m"),
			caseStamp + " second: m\n"},
		{"comm", entryOf("_COMM", "first", "_COMM", "second", "MESSAGE", "m"), caseStamp + " second: m\n"},
		{"pid", entryOf("SYSLOG_IDENTIFIER", "demo", "_PID", "1", "_PID", "2", "MESSAGE", "m"),
			caseStamp + " demo[2]: m\n"},
		{"syslog pid", entryOf("SYSLOG_IDENTIFIER", "demo", "SYSLOG_PID", "1", "SYSLOG_PID", "2", "MESSAGE", "m"),
			caseStamp + " demo[2]: m\n"},
		{"source time", standardEntry("m").withField("_SOURCE_REALTIME_TIMESTAMP", "1600000000000000").
			withField("_SOURCE_REALTIME_TIMESTAMP", "1650000000000000"),
			"2022-04-15T05:20:00.000000+00:00 testhost demo[100]: m\n"},
		{"invalid source time last falls back to the entry time",
			standardEntry("m").withField("_SOURCE_REALTIME_TIMESTAMP", "1650000000000000").
				withField("_SOURCE_REALTIME_TIMESTAMP", "junk"),
			standardPrefix + "m\n"},
	})
}

// withField returns e with one more field stored after its others.
func (e Entry) withField(name, value string) Entry {
	fields := append([]Field(nil), e.Fields...)
	e.Fields = append(fields, Field{Name: name, Value: []byte(value)})
	return e
}

// sourceTimeRow is a _SOURCE_REALTIME_TIMESTAMP value and the stamp the
// entry gets with it.
type sourceTimeRow struct {
	source string
	stamp  string
}

// checkSourceTimes renders a standard entry with each row's source time.
func checkSourceTimes(t *testing.T, rows []sourceTimeRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(fmt.Sprintf("%q", row.source), func(t *testing.T) {
			got, _ := renderString(standardEntry("m").withField("_SOURCE_REALTIME_TIMESTAMP", row.source))
			if want := row.stamp + " testhost demo[100]: m\n"; got != want {
				t.Errorf("got  %q\nwant %q", got, want)
			}
		})
	}
}

func TestAppendText_SourceTimeParsesLikeSystemdSafeAtou64(t *testing.T) {
	const (
		source1700 = "2023-11-14T22:13:20.000000+00:00" // 1700000000000000
		micros1    = "1970-01-01T00:00:00.000001+00:00"
		micros5    = "1970-01-01T00:00:00.000005+00:00"
		micros7    = "1970-01-01T00:00:00.000007+00:00"
		micros16   = "1970-01-01T00:00:00.000016+00:00"
	)
	binary1700 := strconv.FormatUint(1700000000000000, 2)
	checkSourceTimes(t, []sourceTimeRow{
		{"1700000000000000", source1700},
		{" 1700000000000000", source1700},
		{"\n\t\r1700000000000000", source1700},
		{"+1700000000000000", source1700},
		{"0x60a24181e4000", source1700},
		{"0X60A24181E4000", source1700},
		{"0o60242203007440000", source1700},
		{"060242203007440000", source1700},
		{"0b" + binary1700, source1700},
		{"0B" + binary1700, source1700},
		{"1700000000000000\x00junk", source1700},
		{"\f1", micros1},
		{"0O7", micros7},
		{"\v0b101", micros5},
		{"0b0b101", micros5},
		{"  0b101", micros5},
		{"\v0x10", micros16},
		{"\t+0x10", micros16},
		{"\v-18446744073709551615", micros1},
		// Rejected: the entry's own time is shown.
		{"abc", caseStamp},
		{"", caseStamp},
		{"1700000000000000 ", caseStamp},
		{"1700000000000000\n", caseStamp},
		{"08", caseStamp},
		{"0x", caseStamp},
		{"0xg", caseStamp},
		{"0x1g", caseStamp},
		{"0b2", caseStamp},
		{"0o", caseStamp},
		{"0o0x10", caseStamp},
		{"0b0x10", caseStamp},
		{"99999999999999999999", caseStamp},
		{"18446744073709551616", caseStamp},
		{"-1", caseStamp},
		{" -5", caseStamp},
		{"0o-5", caseStamp},
		{"+-1", caseStamp},
		{"-0", caseStamp},
		{"-0x0", caseStamp},
		{"\v-1", caseStamp},
	})
}

func TestAppendText_SourceTimeOutOfRangeFallsBackToEntryTime(t *testing.T) {
	checkSourceTimes(t, []sourceTimeRow{
		{"0", caseStamp},
		{"1", "1970-01-01T00:00:00.000001+00:00"},
		{"36028797018963967", "3111-09-16T23:10:18.963967+00:00"}, // 2^55 - 1
		{"36028797018963968", caseStamp},                          // 2^55
		{"18446744073709551615", caseStamp},                       // 2^64 - 1
	})
}

func TestAppendText_StampIsUTCWithMicrosecondsIn32Bytes(t *testing.T) {
	for _, row := range []struct {
		realtime uint64
		stamp    string
	}{
		{1, "1970-01-01T00:00:00.000001+00:00"},
		{1000000, "1970-01-01T00:00:01.000000+00:00"},
		{951782400123456, "2000-02-29T00:00:00.123456+00:00"},
		{1709164800000000, "2024-02-29T00:00:00.000000+00:00"},
		{caseRealtime, caseStamp},
		{4102444799999999, "2099-12-31T23:59:59.999999+00:00"},
		{1<<55 - 1, "3111-09-16T23:10:18.963967+00:00"},
	} {
		e := entryOf("SYSLOG_IDENTIFIER", "demo", "MESSAGE", "m")
		e.Realtime = row.realtime
		got, shown := renderString(e)
		if !shown || got != row.stamp+" demo: m\n" {
			t.Errorf("realtime %d: got %q, shown %t; want the stamp %q", row.realtime, got, shown, row.stamp)
		}
		if len(row.stamp) != 32 {
			t.Errorf("the stamp %q is %d bytes, want 32", row.stamp, len(row.stamp))
		}
	}
}

func TestAppendText_EntryTimeOutOfRangeWithoutAValidSourceTimeIsNotShown(t *testing.T) {
	for _, row := range []struct {
		name     string
		realtime uint64
		source   []string
		want     string
	}{
		{"zero", 0, nil, ""},
		{"2^55", 1 << 55, nil, ""},
		{"2^64 - 1", math.MaxUint64, nil, ""},
		{"zero with an invalid source time", 0, []string{"junk"}, ""},
		{"zero with a valid source time", 0, []string{"1700000000000000"},
			"2023-11-14T22:13:20.000000+00:00 testhost demo[100]: m\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := standardEntry("m")
			e.Realtime = row.realtime
			for _, source := range row.source {
				e = e.withField("_SOURCE_REALTIME_TIMESTAMP", source)
			}
			got, shown := renderString(e)
			if got != row.want || shown != (row.want != "") {
				t.Errorf("got %q, shown %t; want %q", got, shown, row.want)
			}
		})
	}
}

// identityEntry returns an entry with the message m and the given fields
// before it.
func identityEntry(message string, nameValues ...string) Entry {
	return entryOf(append(nameValues, "MESSAGE", message)...)
}

func TestAppendText_HostnameIsPrintedOnlyWhenUnder300PrintableBytes(t *testing.T) {
	h299, h300 := strings.Repeat("h", 299), strings.Repeat("h", 300)
	withHost := func(host string) Entry {
		return identityEntry("m", "_HOSTNAME", host, "SYSLOG_IDENTIFIER", "demo", "_PID", "100")
	}
	dropped := caseStamp + " demo[100]: m\n"
	checkRows(t, []renderRow{
		{"no hostname", identityEntry("m", "SYSLOG_IDENTIFIER", "demo", "_PID", "100"), dropped},
		{"299 bytes", withHost(h299), caseStamp + " " + h299 + " demo[100]: m\n"},
		{"300 bytes", withHost(h300), dropped},
		{"escape", withHost("te\x1bst"), dropped},
		{"invalid UTF-8", withHost("te\xffst"), dropped},
		{"NUL", withHost("ab\x00cd"), dropped},
		{"color sequence, not stripped", withHost("\x1b[31mred\x1b[0m"), dropped},
		{"carriage return", withHost("host\r"), dropped},
		{"tab, printed raw", withHost("te\tst"), caseStamp + " te\tst demo[100]: m\n"},
		{"newline, printed raw", withHost("te\nst"), caseStamp + " te\nst demo[100]: m\n"},
		{"space", withHost("my host"), caseStamp + " my host demo[100]: m\n"},
		{"empty, a single space", withHost(""), caseStamp + "  demo[100]: m\n"},
	})
}

func TestAppendText_IdentifierIsSyslogIdentifierElseCommElseUnknown(t *testing.T) {
	i299, i300 := strings.Repeat("i", 299), strings.Repeat("i", 300)
	withNames := func(nameValues ...string) Entry {
		return identityEntry("m", append([]string{"_HOSTNAME", "testhost"}, append(nameValues, "_PID", "100")...)...)
	}
	checkRows(t, []renderRow{
		{"both", withNames("SYSLOG_IDENTIFIER", "demo", "_COMM", "commname"), standardPrefix + "m\n"},
		{"comm only", withNames("_COMM", "commname"), caseStamp + " testhost commname[100]: m\n"},
		{"unprintable identifier", withNames("SYSLOG_IDENTIFIER", "de\x1bmo", "_COMM", "commname"),
			caseStamp + " testhost commname[100]: m\n"},
		{"identifier of 300 bytes", withNames("SYSLOG_IDENTIFIER", i300, "_COMM", "commname"),
			caseStamp + " testhost commname[100]: m\n"},
		{"identifier of 299 bytes", withNames("SYSLOG_IDENTIFIER", i299, "_COMM", "commname"),
			caseStamp + " testhost " + i299 + "[100]: m\n"},
		{"unprintable identifier, no comm", withNames("SYSLOG_IDENTIFIER", "de\x1bmo"),
			caseStamp + " testhost unknown[100]: m\n"},
		{"both unprintable", withNames("SYSLOG_IDENTIFIER", "de\x1bmo", "_COMM", "co\xffmm"),
			caseStamp + " testhost unknown[100]: m\n"},
		{"neither", withNames(), caseStamp + " testhost unknown[100]: m\n"},
		{"empty identifier wins over comm", withNames("SYSLOG_IDENTIFIER", "", "_COMM", "commname"),
			caseStamp + " testhost [100]: m\n"},
		{"empty identifier, no pid", identityEntry("m", "_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", ""),
			caseStamp + " testhost : m\n"},
		{"newline, printed raw", withNames("SYSLOG_IDENTIFIER", "de\nmo"), caseStamp + " testhost de\nmo[100]: m\n"},
		{"tab, printed raw", withNames("SYSLOG_IDENTIFIER", "de\tmo"), caseStamp + " testhost de\tmo[100]: m\n"},
	})
}

func TestAppendText_PidIsPidElseSyslogPidElseNothing(t *testing.T) {
	withPids := func(nameValues ...string) Entry {
		return identityEntry("m", append([]string{"_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", "demo"}, nameValues...)...)
	}
	checkRows(t, []renderRow{
		{"syslog pid only", withPids("SYSLOG_PID", "777"), caseStamp + " testhost demo[777]: m\n"},
		{"both", withPids("_PID", "100", "SYSLOG_PID", "777"), standardPrefix + "m\n"},
		{"unprintable pid", withPids("_PID", "1\x1b0", "SYSLOG_PID", "777"), caseStamp + " testhost demo[777]: m\n"},
		{"pid of 300 bytes", withPids("_PID", strings.Repeat("1", 300), "SYSLOG_PID", "777"),
			caseStamp + " testhost demo[777]: m\n"},
		{"no pid", withPids(), caseStamp + " testhost demo: m\n"},
		{"empty pid", withPids("_PID", ""), caseStamp + " testhost demo[]: m\n"},
		{"not a number", withPids("_PID", "abc"), caseStamp + " testhost demo[abc]: m\n"},
		{"no hostname, identifier or pid", identityEntry("m"), caseStamp + " unknown: m\n"},
	})
}

func TestAppendText_TabInTheMessageBecomesEightSpaces(t *testing.T) {
	checkRows(t, []renderRow{
		{"inside", standardEntry("a\tb"), standardPrefix + "a        b\n"},
		{"no tab stops", standardEntry("abcd\tb"), standardPrefix + "abcd        b\n"},
		{"first", standardEntry("\tfirst"), standardPrefix + "        first\n"},
		{"on every line", standardEntry("one\n\ttwo\nth\tree"),
			standardPrefix + "one\n" + indented(standardIndent, "        two") + indented(standardIndent, "th        ree")},
	})
}

func TestAppendText_ColorAndOSCSequencesAreRemovedFromTheMessage(t *testing.T) {
	checkRows(t, []renderRow{
		{"color", standardEntry("\x1b[31mred\x1b[0m end"), standardPrefix + "red end\n"},
		{"bold color", standardEntry("\x1b[1;31mbold\x1b[m end"), standardPrefix + "bold end\n"},
		{"256 colors", standardEntry("\x1b[38;5;196mx\x1b[0m"), standardPrefix + "x\n"},
		{"NUL inside a color sequence", standardEntry("a\x1b[3\x00mb"), standardPrefix + "ab\n"},
		{"hyperlink ended by BEL", standardEntry("x\x1b]8;;http://e.test/\x07link\x1b]8;;\x07y"),
			standardPrefix + "xlinky\n"},
		{"title ended by ESC backslash", standardEntry("x\x1b]0;title\x1b\\y"), standardPrefix + "xy\n"},
	})
}

func TestAppendText_OtherEscapeSequencesStayAndMakeTheMessageABlob(t *testing.T) {
	blob := func(size string) string { return standardPrefix + "[" + size + " blob data]\n" }
	checkRows(t, []renderRow{
		{"erase line, not a color", standardEntry("M07 \x1b[2Kx"), blob("9B")},
		{"private mode", standardEntry("M08 \x1b[?25lx"), blob("11B")},
		{"unterminated color", standardEntry("M09 abc\x1b[31"), blob("11B")},
		{"aborted color, the tab after it expanded", standardEntry("\x1b[12;\tX"), blob("14B")},
		{"unterminated OSC", standardEntry("M13 x\x1b]0;title"), blob("14B")},
		{"UTF-8 inside an OSC", standardEntry("M14 x\x1b]0;t\u00eftle\x07y"), blob("17B")},
		{"charset designation", standardEntry("M15 \x1b(Bx"), blob("8B")},
		{"tab after escape, not expanded", standardEntry("\x1b\t"), blob("2B")},
		{"escape escape", standardEntry("\x1b\x1b[31mX"), blob("7B")},
		{"escape at the end", standardEntry("M16 x\x1b"), blob("6B")},
	})
}

func TestAppendText_CarriageReturnIsDroppedOnlyBeforeLineFeedOrAtTheEnd(t *testing.T) {
	checkRows(t, []renderRow{
		{"at the end", standardEntry("abc\r"), standardPrefix + "abc\n"},
		{"two at the end", standardEntry("abc\r\r"), standardPrefix + "abc\n"},
		{"before a line feed", standardEntry("a\r\nb"), standardPrefix + "a\n" + indented(standardIndent, "b")},
		{"alone", standardEntry("\r"), standardPrefix + "\n"},
		{"every line", standardEntry("one\r\ntwo\r\nthree\r\n"),
			standardPrefix + "one\n" + indented(standardIndent, "two") + indented(standardIndent, "three")},
		{"inside, a blob", standardEntry("M19 a\rb"), standardPrefix + "[7B blob data]\n"},
		{"before a color sequence, a blob", standardEntry("M37 a\r\x1b[31mb"), standardPrefix + "[7B blob data]\n"},
	})
}

func TestAppendText_UnprintableMessageIsABlobSizedAfterStripping(t *testing.T) {
	blob := func(size string) string { return standardPrefix + "[" + size + " blob data]\n" }
	checkRows(t, []renderRow{
		{"NUL", standardEntry("M22 a\x00b"), blob("7B")},
		{"0xff", standardEntry("M23a \xff"), blob("6B")},
		{"overlong", standardEntry("M23b \xc0\xaf"), blob("7B")},
		{"surrogate", standardEntry("M23c \xed\xa0\x80"), blob("8B")},
		{"U+FFFE", standardEntry("M23d \xef\xbf\xbe"), blob("8B")},
		{"U+FDD0", standardEntry("M23e \xef\xb7\x90"), blob("8B")},
		{"U+0085, a C1 control", standardEntry("M23f \xc2\x85"), blob("7B")},
		{"DEL", standardEntry("M23g \x7f"), blob("6B")},
		{"U+10FFFF", standardEntry("M23h \xf4\x8f\xbf\xbf"), blob("9B")},
		{"U+1FFFE", standardEntry("M23n \xf0\x9f\xbf\xbe"), blob("9B")},
		{"above U+10FFFF", standardEntry("M23i \xf4\x90\x80\x80"), blob("9B")},
		{"five-byte form", standardEntry("M23j \xf8\x88\x80\x80\x80"), blob("10B")},
		{"truncated", standardEntry("M23k \xc3"), blob("6B")},
		{"lone continuation byte", standardEntry("M23l \x80"), blob("6B")},
		{"BEL", standardEntry("M35 \x07"), blob("5B")},
		{"form feed and vertical tab", standardEntry("M36 \f\v"), blob("6B")},
		{"one control byte in a multi-line message", standardEntry("L11 one\ntwo\x01\nthree"), blob("18B")},
		{"tab expanded before sizing", standardEntry("\t\x00"), blob("9B")},
		{"color removed before sizing", standardEntry("\x1b[31m\x00"), blob("1B")},
		{"CR before LF removed before sizing", standardEntry("a\r\n\x00"), blob("3B")},
	})
}

func TestAppendText_PrintableUnicodeIsKept(t *testing.T) {
	for _, message := range []string{
		"h\u00e9llo \u2713 \U0001f600",
		"\ufeff \ue000 \u2028 \ufffd",
		"\ufdcf \ufdf0",
		"\u00a0",
		"  spaced  ",
	} {
		got, _ := renderString(standardEntry(message))
		if want := standardPrefix + message + "\n"; got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
}

func TestAppendText_BlobSizeIsSystemdFormatBytes(t *testing.T) {
	for _, row := range []struct {
		size int
		text string
	}{
		{1, "1B"}, {18, "18B"}, {1023, "1023B"}, {1024, "1K"}, {1126, "1K"}, {1127, "1.1K"},
		{1536, "1.5K"}, {2047, "1.9K"}, {10240, "10K"}, {1048575, "1023.9K"}, {1048576, "1M"},
		{1572864, "1.5M"},
	} {
		message := "\x00" + strings.Repeat("x", row.size-1)
		got, _ := renderString(standardEntry(message))
		if want := standardPrefix + "[" + row.text + " blob data]\n"; got != want {
			t.Errorf("size %d: got %q, want %q", row.size, got, want)
		}
	}
}

func TestAppendFormatBytes_TruncatesToOneDecimalInEveryIECUnit(t *testing.T) {
	for _, row := range []struct {
		size uint64
		text string
	}{
		{0, "0B"},
		{1<<30 - 1, "1023.9M"},
		{1 << 30, "1G"},
		{3 << 29, "1.5G"},
		{1429365116107, "1.2T"},
		{8894609264056726, "7.8P"},
		{1 << 60, "1E"},
		{math.MaxUint64 - 1, "15.9E"},
	} {
		if got := string(appendFormatBytes(nil, row.size)); got != row.text {
			t.Errorf("size %d: got %q, want %q", row.size, got, row.text)
		}
	}
}

func TestAppendText_ContinuationLinesAreIndentedByPrefixBytesPlusTwo(t *testing.T) {
	multi := func(prefix string, indent int, e Entry) renderRow {
		return renderRow{
			name:  fmt.Sprintf("indent %d", indent),
			entry: e,
			want:  prefix + "one\n" + indented(indent, "two") + indented(indent, "three"),
		}
	}
	const message = "one\ntwo\nthree"
	rows := []renderRow{
		multi(standardPrefix, 53, standardEntry(message)),
		multi(caseStamp+" demo[100]: ", 44, identityEntry(message, "SYSLOG_IDENTIFIER", "demo", "_PID", "100")),
		multi(caseStamp+"  demo[100]: ", 45,
			identityEntry(message, "_HOSTNAME", "", "SYSLOG_IDENTIFIER", "demo", "_PID", "100")),
		multi(caseStamp+" testhost demo[4242]: ", 54,
			identityEntry(message, "_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", "demo", "SYSLOG_PID", "4242")),
		multi(caseStamp+" testhost [100]: ", 49,
			identityEntry(message, "_HOSTNAME", "testhost", "SYSLOG_IDENTIFIER", "", "_PID", "100")),
		multi(caseStamp+" a\tb demo[100]: ", 48,
			identityEntry(message, "_HOSTNAME", "a\tb", "SYSLOG_IDENTIFIER", "demo", "_PID", "100")),
		// " unknown" is not counted: the indent is 8 bytes short of the
		// visible prefix.
		multi(caseStamp+" testhost unknown[100]: ", 48, identityEntry(message, "_HOSTNAME", "testhost", "_PID", "100")),
		// Bytes, not columns: "h\u00f4st" is 5 bytes.
		multi(caseStamp+" h\u00f4st demo[100]: ", 50,
			identityEntry(message, "_HOSTNAME", "h\u00f4st", "SYSLOG_IDENTIFIER", "demo", "_PID", "100")),
	}
	for i := range rows {
		rows[i].name += fmt.Sprintf(" (row %d)", i)
	}
	checkRows(t, rows)
}

func TestAppendText_OneTrailingNewlineIsSwallowedAndEmptyPiecesAreIndented(t *testing.T) {
	ind := func(text string) string { return indented(standardIndent, text) }
	checkRows(t, []renderRow{
		{"empty", standardEntry(""), standardPrefix + "\n"},
		{"one newline", standardEntry("\n"), standardPrefix + "\n"},
		{"trailing newline", standardEntry("one\ntwo\n"), standardPrefix + "one\n" + ind("two")},
		{"two trailing newlines", standardEntry("one\n\n"), standardPrefix + "one\n" + ind("")},
		{"empty middle piece", standardEntry("one\n\nthree"), standardPrefix + "one\n" + ind("") + ind("three")},
		{"leading newline", standardEntry("\nsecond"), standardPrefix + "\n" + ind("second")},
		{"three newlines", standardEntry("\n\n\n"), standardPrefix + "\n" + ind("") + ind("")},
		{"ten lines", standardEntry("1\n2\n3\n4\n5\n6\n7\n8\n9\n10"),
			standardPrefix + "1\n" + ind("2") + ind("3") + ind("4") + ind("5") + ind("6") + ind("7") +
				ind("8") + ind("9") + ind("10")},
	})
}

func TestAppendText_AppendsAfterTheBytesAlreadyInDst(t *testing.T) {
	const earlier = "an earlier entry\n"
	for _, message := range []string{"one line", "one\n\ntwo\n\n", "\x1b[31mred\x1b[0m\tx", "a\x00b", ""} {
		alone, _ := AppendText(nil, standardEntry(message))
		for _, capacity := range []int{len(earlier), 1 << 16} {
			dst := append(make([]byte, 0, capacity), earlier...)
			got, shown := AppendText(dst, standardEntry(message))
			if !shown || string(got) != earlier+string(alone) {
				t.Errorf("message %q, capacity %d: got %q, want %q", message, capacity, got, earlier+string(alone))
			}
		}
	}
}

// longMessage returns a message of exactly messageBytes bytes in lines
// lines, without a final newline: "LINE 000000" and dashes up to an even
// width, the first lines one byte longer than the others.
func longMessage(messageBytes, lines int) []byte {
	content := messageBytes - (lines - 1)
	width, longer := content/lines, content%lines
	out := make([]byte, 0, messageBytes)
	for i := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		line := fmt.Appendf(nil, "LINE %06d", i)
		lineWidth := width
		if i < longer {
			lineWidth++
		}
		out = append(out, line...)
		out = append(out, bytes.Repeat([]byte{'-'}, lineWidth-len(line))...)
	}
	return out
}

// expectedLines is the text of a standard entry whose message is the
// given lines, built line by line.
func expectedLines(message []byte) []byte {
	out := []byte(standardPrefix)
	for i, line := range bytes.Split(message, []byte("\n")) {
		if i > 0 {
			out = append(out, strings.Repeat(" ", standardIndent)...)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}

// allocatedBytes returns how many bytes of heap memory run allocated.
// runtime.ReadMemStats stops the world to read the allocator's counters;
// TotalAlloc only grows, so the difference is what run allocated (and,
// negligibly here, anything another goroutine allocated meanwhile).
func allocatedBytes(run func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestAppendText_FourMebibyteMessageOf200000LinesRendersInOneAllocation(t *testing.T) {
	const (
		messageBytes = 4 << 20
		lines        = 200_000
	)
	message := longMessage(messageBytes, lines)
	if len(message) != messageBytes || bytes.Count(message, []byte("\n")) != lines-1 {
		t.Fatalf("the message is %d bytes in %d lines", len(message), bytes.Count(message, []byte("\n"))+1)
	}
	e := standardEntry("")
	e.Fields[len(e.Fields)-1].Value = message
	want := expectedLines(message)
	if wantLen := standardIndent + messageBytes + (lines-1)*standardIndent + 1; len(want) != wantLen {
		t.Fatalf("expected text is %d bytes, want %d", len(want), wantLen)
	}

	var out []byte
	allocated := allocatedBytes(func() { out, _ = AppendText(nil, e) })
	if !bytes.Equal(out, want) {
		t.Fatal(firstDifference(out, want))
	}
	// From an empty dst the renderer reserves its whole text once, sized
	// from the message before stripping, so the one buffer is at most
	// the text plus what stripping removed (nothing here) plus a line's
	// indent and the blob line's room.
	if limit := uint64(2 * len(out)); allocated > limit {
		t.Errorf("allocated %d bytes for a %d-byte text, want at most %d (twice the text)", allocated, len(out), limit)
	}
	t.Logf("a %d-byte text allocated %d bytes from an empty dst", len(out), allocated)
	if allocs := testing.AllocsPerRun(3, func() { out, _ = AppendText(nil, e) }); allocs != 1 {
		t.Errorf("%.0f allocations from an empty dst, want 1", allocs)
	}
	buffer := make([]byte, 0, 2*len(want))
	if allocs := testing.AllocsPerRun(3, func() { buffer, _ = AppendText(buffer[:0], e) }); allocs != 0 {
		t.Errorf("%.0f allocations into a dst with room, want 0", allocs)
	}
}

func TestAppendText_OrdinaryEntryAllocatesNothingIntoADstWithRoom(t *testing.T) {
	e := standardEntry("service started\twith a \x1b[1mbold\x1b[0m word\nand a second line")
	buffer := make([]byte, 0, 4096)
	if allocs := testing.AllocsPerRun(100, func() { buffer, _ = AppendText(buffer[:0], e) }); allocs != 0 {
		t.Errorf("%.0f allocations per entry, want 0", allocs)
	}
}
