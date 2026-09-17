package trace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A line that is not valid UTF-8 is answered with its own bytes, the
// way samples and a trace-cache hit read them from the file. The JSON
// encoder writes each byte that is not part of a valid character as
// U+FFFD, so every surface shows the same text; submatch positions
// count the line's bytes.

// lossyText is the text a JSON answer carries for s: s with each byte
// that is not part of a valid UTF-8 character replaced by U+FFFD, as
// encoding/json writes it.
func lossyText(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	return decoded
}

// The cut keeps at most n bytes and never splits a valid character;
// a byte that belongs to no valid character is a character of its own.
func TestCharacterCut_NeverSplitsAValidCharacter(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
		want int
	}{
		{"an ASCII byte at the cut", "abcdef", 3, 3},
		{"the cut inside a two-byte character", "abé", 3, 2},
		{"the cut inside a four-byte character", "a😀b", 3, 1},
		{"a lead byte at the cut", "ab€", 2, 2},
		{"a stray continuation byte after a whole character", "e€\x80\x80", 4, 4},
		{"stray continuation bytes only", "\x80\x80\x80\x80\x80", 4, 4},
		{"a lead byte whose sequence is broken", "\xe2\x82A", 1, 1},
		{"a valid prefix cut short by the end of the kept bytes", "abc\xf0\x9f", 4, 3},
		{"a character that is U+FFFD itself", "a�b", 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := characterCut([]byte(tc.text), tc.n); got != tc.want {
				t.Errorf("characterCut(%q, %d) = %d, want %d", tc.text, tc.n, got, tc.want)
			}
		})
	}
}

// corruptLine overwrites bytes of the 1-based line n in place with
// sequences that are not valid UTF-8 (a stray \xff\xfe pair, a
// character cut short) and a valid two-byte "é", all inside the line's
// random payload, so no byte moves. On a needle line the first two
// bytes become a character cut short too, so NEEDLE comes after bytes
// whose characters do not count as bytes do.
func corruptLine(text []byte, table textLineTable, n int) {
	start := table.starts[n-1]
	line := text[start : start+int64(len(table.texts[n-1]))]
	payload := strings.LastIndexByte(string(line), ' ') + 1
	copy(line[payload+3:], "\xff\xfe")
	copy(line[payload+10:], "\xe2\x82")
	copy(line[payload+20:], "é")
	if strings.Contains(string(line), "NEEDLE") {
		copy(line, "\xe2\x82")
	}
}

// invalidUTF8Fixture writes a plain log of about size bytes with a
// needle on every 97th line and returns its path and line table. Every
// other needle line, and the lines on either side of it, hold bytes
// that are not valid UTF-8.
func invalidUTF8Fixture(t *testing.T, size int) (string, textLineTable) {
	t.Helper()
	text := writeNeedleText(size, func(line int) bool { return line%97 == 0 })
	table := newTextLineTable(text)
	for n := 194; n+1 <= len(table.starts); n += 194 {
		for _, line := range []int{n - 1, n, n + 1} {
			corruptLine(text, table, line)
		}
	}
	table = newTextLineTable(text)
	return writeTextFile(t, "invalid-utf8.log", text), table
}

// requireLinesAsInTheFile checks each match of resp against the file's
// text: line_text is the line's bytes, and its one submatch is NEEDLE at
// its byte position in the line. It returns how many matched lines are
// not valid UTF-8, which a fixture must make more than zero.
func requireLinesAsInTheFile(t *testing.T, resp *rxtypes.TraceResponse, table textLineTable) (invalid int) {
	t.Helper()
	for _, m := range resp.Matches {
		i, ok := table.byStart[m.Offset]
		if !ok {
			t.Fatalf("match at offset %d is not at the start of a line", m.Offset)
		}
		want := table.texts[i]
		if derefText(m.LineText) != want {
			t.Errorf("line %d: line_text %q, want %q", i+1, derefText(m.LineText), want)
		}
		wantSub := rxtypes.Submatch{Text: "NEEDLE", Start: strings.Index(want, "NEEDLE"), End: strings.Index(want, "NEEDLE") + len("NEEDLE")}
		if len(m.Submatches) != 1 || m.Submatches[0] != wantSub {
			t.Errorf("line %d: submatches %+v, want [%+v]", i+1, m.Submatches, wantSub)
		}
		if lossyText(t, want) != want {
			invalid++
		}
	}
	return invalid
}

// Scenarios: the line text of a match and of a context line is the
// line's bytes, whichever way the file is stored, and a submatch's
// start and end are byte positions in that line.
func TestInvalidUTF8LinesAnswerTheirOwnBytesOnEveryStoredForm(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	plain, table := invalidUTF8Fixture(t, 600_000)
	text := []byte(strings.Join(table.texts, "\n") + "\n")
	opts := Options{ContextBefore: 1, ContextAfter: 1, NoCache: true}
	for _, form := range storedForms() {
		t.Run(form.name, func(t *testing.T) {
			path := plain
			if form.name != "plain" {
				path = form.write(t, text)
			}
			resp := traceOnce(t, path, []string{"NEEDLE"}, opts)
			if invalid := requireLinesAsInTheFile(t, resp, table); invalid == 0 {
				t.Fatal("no matched line in the answer is invalid UTF-8; the fixture tests nothing")
			}
			checkWindowsByOffset(t, resp, table, 1, 1)
		})
	}
}

// Scenario: a scan, a trace-cache hit and samples give an invalid line
// the same text, and its JSON holds one U+FFFD per byte that is not
// part of a valid character.
func TestInvalidUTF8LinesAgreeOnAScanACacheHitAndSamples(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LARGE_FILE_MB", "1")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	path, table := invalidUTF8Fixture(t, 3_000_000)
	opts := Options{ContextBefore: 1, ContextAfter: 1}

	scanned := traceOnce(t, path, []string{"NEEDLE"}, opts)
	requireTraceCache(t, path)
	if invalid := requireLinesAsInTheFile(t, scanned, table); invalid == 0 {
		t.Fatal("no matched line in the answer is invalid UTF-8; the fixture tests nothing")
	}
	cached := traceFromCacheWith(t, path, opts)
	traceanswer.RequireSame(t, "cache hit", cached, scanned)
	requireSamplesInvertTrace(t, path, scanned)

	i := 194 - 1
	encoded, err := json.Marshal(scanned.Matches[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Offset   int64  `json:"offset"`
		LineText string `json:"line_text"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Offset != table.starts[i] {
		t.Fatalf("second match at offset %d, want line %d at %d", decoded.Offset, i+1, table.starts[i])
	}
	if want := lossyText(t, table.texts[i]); decoded.LineText != want || strings.Count(want, "�") != 6 {
		t.Errorf("JSON line_text %q, want %q with six U+FFFD", decoded.LineText, want)
	}
}

// Scenario: pattern identification reads an invalid line's own bytes,
// so each line is credited to the pattern that matches it and to no
// other.
func TestInvalidUTF8LineIsCreditedToThePatternThatMatchesIt(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path, table := invalidUTF8Fixture(t, 100_000)
	resp := traceOnce(t, path, []string{"NEEDLE", "filler"}, Options{NoCache: true})
	if len(resp.Matches) != len(table.texts) {
		t.Fatalf("%d matches, want one per line (%d)", len(resp.Matches), len(table.texts))
	}
	for _, m := range resp.Matches {
		line := table.texts[table.byStart[m.Offset]]
		want := "p2"
		if strings.Contains(line, "NEEDLE") {
			want = "p1"
		}
		if m.Pattern != want {
			t.Errorf("line at offset %d (%q) credited to %s, want %s", m.Offset, line, m.Pattern, want)
		}
	}
}

// Scenarios: the report's file. A CRLF break is stripped after the
// line's bytes are decoded, a valid UTF-8 line is unchanged, and the
// submatch of the invalid line starts at its byte position.
func TestInvalidUTF8LineKeepsItsBytesAndLosesItsCRLF(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	path := writeTextFile(t, "x.log", []byte("a\r\nERR one\r\n\xff\xfe ERR bad\r\ncafé ERR →\nlast ERR"))
	resp := traceOnce(t, path, []string{"ERR"}, Options{ContextBefore: 1, NoCache: true})

	want := []struct {
		offset int64
		text   string
		start  int
	}{
		{3, "ERR one", 0},
		{12, "\xff\xfe ERR bad", 3},
		{24, "café ERR →", 6},
		{38, "last ERR", 5},
	}
	if len(resp.Matches) != len(want) {
		t.Fatalf("%d matches, want %d", len(resp.Matches), len(want))
	}
	for i, w := range want {
		m := resp.Matches[i]
		wantSub := rxtypes.Submatch{Text: "ERR", Start: w.start, End: w.start + 3}
		if m.Offset != w.offset || derefText(m.LineText) != w.text || len(m.Submatches) != 1 || m.Submatches[0] != wantSub {
			t.Errorf("match %d: offset %d, line_text %q, submatches %+v; want %d, %q, [%+v]",
				i, m.Offset, derefText(m.LineText), m.Submatches, w.offset, w.text, wantSub)
		}
	}
	window := resp.ContextLines[fmt.Sprintf("p1:%s:12", resp.Matches[1].File)]
	if len(window) != 2 || window[0].LineText != "ERR one" || window[1].LineText != "\xff\xfe ERR bad" {
		t.Errorf("window of the invalid line: %+v, want the lines \"ERR one\" and %q", window, "\xff\xfe ERR bad")
	}
	requireSamplesInvertTrace(t, path, resp)
}

// Scenario: a long invalid line cut by RX_MAX_LINE_TEXT_BYTES keeps the
// most bytes that end on a character of its text, the same on a scan
// and on a cache hit, and its cut text reads as the start of its whole
// text.
func TestLongInvalidUTF8LineIsCutOnACharacterAlikeOnAScanAndACacheHit(t *testing.T) {
	requireRipgrep(t)
	const limit = 4096
	cases := []struct {
		name    string
		atCut   string // bytes placed so the limit falls on the second byte
		wantCut int
	}{
		{"stray continuation bytes at the limit", "\x80\x80", limit},
		{"a character across the limit", "\xe2\x82\xac", limit - 1},
		{"an invalid pair across the limit", "\xff\xfe", limit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_CACHE_DIR", t.TempDir())
			t.Setenv("RX_LARGE_FILE_MB", "1")
			head := "LINE 1 NEEDLE \xff "
			first := head + strings.Repeat("y", limit-1-len(head)) + tc.atCut + strings.Repeat("y\xfe", 750_000)
			var b strings.Builder
			b.WriteString(first + "\n")
			for i := 2; i <= 3000; i++ {
				fmt.Fprintf(&b, "LINE %d short\n", i)
			}
			b.WriteString("LINE 3001 NEEDLE\n")
			path := writeTextFile(t, "cut.log", []byte(b.String()))
			opts := Options{ContextBefore: 1, ContextAfter: 1}

			setLineLimits(t, 1<<30, 1<<30)
			traceOnce(t, path, []string{"NEEDLE"}, opts)
			requireTraceCache(t, path)

			setLineLimits(t, limit, 100)
			scanned := traceWithin(t, path, []string{"NEEDLE"}, opts)
			got := scanned.Matches[0]
			if derefText(got.LineText) != first[:tc.wantCut] || !got.LineTextTruncated {
				t.Errorf("scan keeps %d bytes (truncated %v), want %d", len(derefText(got.LineText)), got.LineTextTruncated, tc.wantCut)
			}
			if !strings.HasPrefix(lossyText(t, first), lossyText(t, derefText(got.LineText))) {
				t.Error("the cut text does not read as the start of the whole line's text")
			}
			cached := cappedTraceFromCache(t, path, []string{"NEEDLE"}, opts)
			traceanswer.RequireSame(t, "cache hit", cached, scanned)
		})
	}
}
