package timestamps

import (
	"strings"
	"testing"
)

// Text gives the timestamp of a line as the line writes it, for the
// playground's shapes and every family.
func TestText_GivesTheTimestampAsWritten(t *testing.T) {
	cases := []struct {
		name   string
		format Format
		line   string
		want   string
	}{
		{"iso with dot millis", isoAnchored, "2025-12-10 07:00:04.574 [1765375204574] [PatchStream] INFO", "2025-12-10 07:00:04.574"},
		{"iso with one-digit fields and colon millis", isoAnchored, "2025-2-15 18:16:22:397 (field_trial.cc:164): Setting", "2025-2-15 18:16:22:397"},
		{"iso with comma millis", isoAnchored, "2025-12-10 16:18:53,741 INFO root: started", "2025-12-10 16:18:53,741"},
		{"iso with a zone word", isoZoned, "2025-12-10 07:49:50 UTC [123]: LOG:  statement", "2025-12-10 07:49:50 UTC"},
		{"iso with a numeric zone", isoZoned, "2026-10-06T12:34:56.123+02:00 msg", "2026-10-06T12:34:56.123+02:00"},
		{"iso after a bracket", isoAnchored, "[2025-12-10 07:00:04.574] worker started", "2025-12-10 07:00:04.574"},
		{"iso windowed", isoWindowed, "level=info ts=2025-12-10T07:00:04Z msg=x", "2025-12-10T07:00:04Z"},
		{"syslog", syslogAnchored, "Dec 10 07:00:12.156 F747D4634A6C I      vol.streaming.xcopy", "Dec 10 07:00:12.156"},
		{"clf", clfWindowed, `203.0.113.7 - - [06/Oct/2026:12:34:56 +0000] "GET /index.html HTTP/1.1" 200`, "[06/Oct/2026:12:34:56 +0000]"},
		{"ctime", ctimeAnchored, "[Tue Oct 06 12:34:56.123456 2026] [core:error] [pid 4242]", "Tue Oct 06 12:34:56.123456 2026"},
		{"slash with AM/PM", slashMonthFirst, "10/06/2026 12:34:56 PM INFO [worker-7] job 42", "10/06/2026 12:34:56 PM"},
		{"dotted", dottedAnchored, "06.10.2026 12:34:56,789 INFO  [main] de.example.App", "06.10.2026 12:34:56,789"},
		{"epoch", epochAnchored, "1696600000.123 level=info msg=served", "1696600000.123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustParser(t, tc.format, mtime2025)
			got, ok := p.Text([]byte(tc.line))
			if !ok || got != tc.want {
				t.Fatalf("Text(%q) = %q, %t; want %q", tc.line, got, ok, tc.want)
			}
		})
	}
}

// A line without a timestamp has no text.
func TestText_LineWithoutATimestampHasNone(t *testing.T) {
	p := mustParser(t, isoAnchored, mtime2025)
	if got, ok := p.Text([]byte("\tat com.example.Main.run(Main.java:42)")); ok || got != "" {
		t.Fatalf("Text = %q, %t; want none", got, ok)
	}
}

// Locate finds the same timestamp Own does, and a span inside the
// window that Text cuts the line at.
func TestLocate_AgreesWithOwn(t *testing.T) {
	for _, bl := range ownBenchLines {
		p := mustParser(t, bl.format, mtime2025)
		line := []byte(bl.line)
		want, wantOK := p.Own(line)
		got, span, ok := p.Locate(line)
		if ok != wantOK || got != want {
			t.Errorf("%s: Locate = %+v, %t; Own = %+v, %t", bl.format.Family, got, ok, want, wantOK)
		}
		if ok && (span.Start < 0 || span.End <= span.Start || span.End > min(len(line), WindowBytes)) {
			t.Errorf("%s: span %+v outside the window of a %d-byte line", bl.format.Family, span, len(line))
		}
	}
}

// escapeText escapes every byte that is not printable ASCII and stops
// before the text would pass MaxTextBytes, never inside an escape.
func TestEscapeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"printable text is kept", "2025-12-10 07:00:04.574", "2025-12-10 07:00:04.574"},
		{"control bytes are escaped", "a\tb\x00c\x7f", `a\x09b\x00c\x7f`},
		{"bytes above ASCII are escaped", "x\xc3\xa9", `x\xc3\xa9`},
		{"a backslash is escaped", `a\b`, `a\x5cb`},
		{"long text is cut", strings.Repeat("9", 70), strings.Repeat("9", MaxTextBytes)},
		{"an escape is never cut", strings.Repeat("9", MaxTextBytes-2) + "\x01", strings.Repeat("9", MaxTextBytes-2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeText([]byte(tc.in)); got != tc.want {
				t.Fatalf("escapeText(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
