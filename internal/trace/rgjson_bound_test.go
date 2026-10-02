package trace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// setLineLimits sets the two per-line bounds for the rest of the test.
func setLineLimits(t *testing.T, lineTextBytes, submatches int) {
	t.Helper()
	t.Setenv("RX_MAX_LINE_TEXT_BYTES", strconv.Itoa(lineTextBytes))
	t.Setenv("RX_MAX_SUBMATCHES_PER_LINE", strconv.Itoa(submatches))
}

// generatedReader produces a stream piece by piece, so a test can feed
// the parser an event far larger than anything it allocates itself.
// next returns the next piece, or nil at the end.
type generatedReader struct {
	next    func() []byte
	pending []byte
}

func (g *generatedReader) Read(p []byte) (int, error) {
	for len(g.pending) == 0 {
		g.pending = g.next()
		if g.pending == nil {
			return 0, io.EOF
		}
	}
	n := copy(p, g.pending)
	g.pending = g.pending[n:]
	return n, nil
}

// hugeMatchEvent is the event ripgrep writes for line lineNumber, at
// absoluteOffset, when the line is "LINE 7 " followed by xCount x's and
// the pattern is `x`: the whole line, then one submatch per x. Nothing
// but a 64 KiB block of x's and one block of submatches is held at once.
//
// The first 1,024 submatches carry their real positions; the rest
// repeat one prepared block, which keeps the generator cheap. The
// parser keeps none of them, so their positions do not matter.
func hugeMatchEvent(lineNumber int, absoluteOffset int64, xCount int) io.Reader {
	const prefix = "LINE 7 "
	const exactSubmatches = 1024
	block := bytes.Repeat([]byte("x"), 64*1024)
	var exact []byte
	for i := 0; i < exactSubmatches; i++ {
		start := len(prefix) + i
		exact = fmt.Appendf(exact, `{"match":{"text":"x"},"start":%d,"end":%d},`, start, start+1)
	}
	const repeatedPerBlock = 1024
	repeated := bytes.Repeat([]byte(`{"match":{"text":"x"},"start":9,"end":10},`), repeatedPerBlock)
	stage, written, subs := 0, 0, 0
	return &generatedReader{next: func() []byte {
		switch stage {
		case 0:
			stage++
			return []byte(`{"type":"match","data":{"path":{"text":"<stdin>"},"lines":{"text":"` + prefix)
		case 1:
			if written < xCount {
				n := min(len(block), xCount-written)
				written += n
				return block[:n]
			}
			stage++
			return []byte(fmt.Sprintf(`\n"},"line_number":%d,"absolute_offset":%d,"submatches":[`, lineNumber, absoluteOffset))
		case 2:
			stage++
			subs = exactSubmatches
			return exact
		case 3:
			if subs+repeatedPerBlock < xCount {
				subs += repeatedPerBlock
				return repeated
			}
			stage++
			// The last submatch has no comma after it.
			return []byte(`{"match":{"text":"x"},"start":9,"end":10}]}}` + "\n")
		case 4:
			stage++
			return []byte(`{"type":"match","data":{"path":{"text":"<stdin>"},"lines":{"text":"LINE 8 x\n"},"line_number":8,"absolute_offset":1,"submatches":[{"match":{"text":"x"},"start":7,"end":8}]}}` + "\n")
		}
		return nil
	}}
}

// An event of any size is read in bounded memory: the line's text is
// cut at RX_MAX_LINE_TEXT_BYTES and its submatches at
// RX_MAX_SUBMATCHES_PER_LINE, while the line's size, number and offset,
// which ripgrep writes after the text, stay exact. The event after it
// is read as usual.
func TestStreamEvents_ReadsAHugeEventInBoundedMemory(t *testing.T) {
	setLineLimits(t, 4096, 100)
	const xCount = 16 << 20 // a 16 MiB line and 16 Mi submatches: an event of about 700 MB
	stream := hugeMatchEvent(7, 123_456_789_012, xCount)

	var events []*RgEvent
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := StreamEvents(context.Background(), stream, func(ev *RgEvent, parseErr error) error {
		if parseErr != nil {
			return parseErr
		}
		events = append(events, ev)
		return nil
	})
	runtime.ReadMemStats(&after)

	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("reading the event allocated %d KiB", allocated>>10)
	if allocated > 8<<20 {
		t.Errorf("reading the event allocated %d MiB, want under 8 MiB", allocated>>20)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	m := events[0].Match
	if m.LineNumber != 7 || m.AbsoluteOffset != 123_456_789_012 {
		t.Errorf("line %d at offset %d, want line 7 at offset 123456789012", m.LineNumber, m.AbsoluteOffset)
	}
	if want := len("LINE 7 ") + xCount + 1; m.Lines.Size != want {
		t.Errorf("line size %d, want %d", m.Lines.Size, want)
	}
	if want := "LINE 7 " + strings.Repeat("x", 4096-len("LINE 7 ")); m.Lines.Text != want || !m.Lines.Truncated {
		t.Errorf("line text of %d bytes (truncated %v), want the first 4096 bytes, truncated", len(m.Lines.Text), m.Lines.Truncated)
	}
	if len(m.Submatches) != 100 || !m.SubmatchesTruncated {
		t.Errorf("%d submatches (truncated %v), want 100, truncated", len(m.Submatches), m.SubmatchesTruncated)
	}
	if last := m.Submatches[len(m.Submatches)-1]; last.Start != 106 || last.End != 107 || last.Text() != "x" {
		t.Errorf("last submatch kept %+v, want x at 106-107", last)
	}
	if next := events[1].Match; next.LineNumber != 8 || next.Lines.Text != "LINE 8 x\n" || next.Lines.Truncated {
		t.Errorf("event after the huge one: %+v", next)
	}
}

// boundCase is one event and what the bounded parser makes of it, with
// line text cut at 8 bytes and at most 3 submatches kept.
type boundCase struct {
	name      string
	event     string
	wantText  string
	wantSize  int
	wantCut   bool
	wantSubs  []RgSubmatch
	wantSubsT bool
}

// matchEvent builds a match event with the given lines object and
// submatch list (raw JSON).
func matchEvent(lines, submatches string) string {
	return `{"type":"match","data":{"path":{"text":"<stdin>"},"lines":` + lines +
		`,"line_number":3,"absolute_offset":40,"submatches":[` + submatches + `]}}`
}

func sub(text string, start, end int) RgSubmatch {
	return RgSubmatch{Match: RgText{Text: text, Size: len(text)}, Start: start, End: end}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func boundCases() []boundCase {
	return []boundCase{
		{
			name: "a line under the limit is unchanged", event: matchEvent(`{"text":"ab c\n"}`, `{"match":{"text":"c"},"start":3,"end":4}`),
			wantText: "ab c\n", wantSize: 5, wantSubs: []RgSubmatch{sub("c", 3, 4)},
		},
		{
			name: "a line exactly at the limit is unchanged", event: matchEvent(`{"text":"abcdefgh\n"}`, ``),
			wantText: "abcdefgh\n", wantSize: 9, wantSubs: []RgSubmatch{},
		},
		{
			name: "a CRLF line at the limit is unchanged", event: matchEvent(`{"text":"abcdefgh\r\n"}`, ``),
			wantText: "abcdefgh\r\n", wantSize: 10, wantSubs: []RgSubmatch{},
		},
		{
			name: "a last line without a break at the limit is unchanged", event: matchEvent(`{"text":"abcdefgh"}`, ``),
			wantText: "abcdefgh", wantSize: 8, wantSubs: []RgSubmatch{},
		},
		{
			name: "a line one byte over the limit is cut", event: matchEvent(`{"text":"abcdefghi\n"}`, ``),
			wantText: "abcdefgh", wantSize: 10, wantCut: true, wantSubs: []RgSubmatch{}, wantSubsT: true,
		},
		{
			name: "the cut moves back to the start of a character", event: matchEvent(`{"text":"abcdefgéz\n"}`, ``),
			wantText: "abcdefg", wantSize: 11, wantCut: true, wantSubs: []RgSubmatch{}, wantSubsT: true,
		},
		{
			name: "escapes count as the bytes they stand for", event: matchEvent(`{"text":"a\"b\\cé😀\n"}`, ``),
			wantText: "a\"b\\cé", wantSize: 12, wantCut: true, wantSubs: []RgSubmatch{}, wantSubsT: true,
		},
		{
			name: "a lone surrogate reads as U+FFFD, as encoding/json reads it", event: matchEvent(`{"text":"\ud83dZ\n"}`, ``),
			wantText: "\ufffdZ\n", wantSize: 5, wantSubs: []RgSubmatch{},
		},
		{
			name: "a base64 line under the limit is unchanged", event: matchEvent(`{"bytes":"`+b64("ab\xff\n")+`"}`, ``),
			wantText: b64("ab\xff\n"), wantSize: 4, wantSubs: []RgSubmatch{},
		},
		{
			name: "a base64 line is cut at the limit", event: matchEvent(`{"bytes":"`+b64("0123456789\xff\n")+`"}`, ``),
			wantText: b64("01234567"), wantSize: 12, wantCut: true, wantSubs: []RgSubmatch{}, wantSubsT: true,
		},
		{
			name:     "submatches past the cap are left out",
			event:    matchEvent(`{"text":"aaaaa\n"}`, `{"match":{"text":"a"},"start":0,"end":1},{"match":{"text":"a"},"start":1,"end":2},{"match":{"text":"a"},"start":2,"end":3},{"match":{"text":"a"},"start":3,"end":4}`),
			wantText: "aaaaa\n", wantSize: 6,
			wantSubs: []RgSubmatch{sub("a", 0, 1), sub("a", 1, 2), sub("a", 2, 3)}, wantSubsT: true,
		},
		{
			name:     "a cut line keeps the submatches that start inside it",
			event:    matchEvent(`{"text":"xxxxxxxxxxxx\n"}`, `{"match":{"text":"xx"},"start":0,"end":2},{"match":{"text":"xxxx"},"start":6,"end":10},{"match":{"text":"xx"},"start":10,"end":12}`),
			wantText: "xxxxxxxx", wantSize: 13, wantCut: true,
			wantSubs: []RgSubmatch{sub("xx", 0, 2), {Match: RgText{Text: "xx", Size: 4, Truncated: true}, Start: 6, End: 10}}, wantSubsT: true,
		},
		{
			name:     "fields in another order give the same answer",
			event:    `{"data":{"submatches":[{"end":10,"start":6,"match":{"text":"xxxx"}},{"start":10,"end":12,"match":{"text":"xx"}}],"absolute_offset":40,"lines":{"text":"xxxxxxxxxxxx\n"},"line_number":3,"path":{"text":"<stdin>"}},"type":"match"}`,
			wantText: "xxxxxxxx", wantSize: 13, wantCut: true,
			wantSubs: []RgSubmatch{{Match: RgText{Text: "xx", Size: 4, Truncated: true}, Start: 6, End: 10}}, wantSubsT: true,
		},
	}
}

// A line longer than RX_MAX_LINE_TEXT_BYTES is cut at the start of the
// character holding that byte, in both payload forms; a line at the
// limit or under it is left alone. Submatches are kept up to
// RX_MAX_SUBMATCHES_PER_LINE and only where they start inside the text
// kept, and a cut line always marks its submatches as possibly
// incomplete. The size, number and offset are the whole line's.
func TestParseEvent_BoundsTheLineAndItsSubmatches(t *testing.T) {
	setLineLimits(t, 8, 3)
	for _, tc := range boundCases() {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseEvent([]byte(tc.event))
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			m := ev.Match
			if m.Lines.Text != tc.wantText || m.Lines.Size != tc.wantSize || m.Lines.Truncated != tc.wantCut {
				t.Errorf("lines = %q size %d truncated %v, want %q size %d truncated %v",
					m.Lines.Text, m.Lines.Size, m.Lines.Truncated, tc.wantText, tc.wantSize, tc.wantCut)
			}
			if m.LineNumber != 3 || m.AbsoluteOffset != 40 {
				t.Errorf("line %d at %d, want line 3 at 40", m.LineNumber, m.AbsoluteOffset)
			}
			if !reflect.DeepEqual(m.Submatches, tc.wantSubs) || m.SubmatchesTruncated != tc.wantSubsT {
				t.Errorf("submatches = %+v truncated %v, want %+v truncated %v",
					m.Submatches, m.SubmatchesTruncated, tc.wantSubs, tc.wantSubsT)
			}
		})
	}
}

// A context line is cut like a matched one.
func TestParseEvent_CutsALongContextLine(t *testing.T) {
	setLineLimits(t, 4, 3)
	ev, err := ParseEvent([]byte(`{"type":"context","data":{"path":{"text":"<stdin>"},"lines":{"text":"abcdef\n"},"line_number":2,"absolute_offset":9,"submatches":[]}}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	c := ev.Context
	if c.Lines.Text != "abcd" || !c.Lines.Truncated || c.Lines.Size != 7 || c.LineNumber != 2 || c.AbsoluteOffset != 9 {
		t.Errorf("context = %+v, want text abcd, truncated, size 7, line 2 at 9", c)
	}
}

// Output that is not a ripgrep event, or stops in the middle of one,
// is reported as malformed rather than read as something else.
func TestStreamEvents_ReportsOutputThatIsNotAnEvent(t *testing.T) {
	cases := map[string]string{
		"not JSON":              "this is not json\n",
		"cut off in a string":   `{"type":"match","data":{"lines":{"text":"abc`,
		"cut off after a value": `{"type":"match","data":{"line_number":3`,
		"a number that is text": `{"type":"match","data":{"line_number":"seven"}}` + "\n",
		"a bad escape":          `{"type":"match","data":{"lines":{"text":"a\q"}}}` + "\n",
		"a control character":   "{\"type\":\"match\",\"data\":{\"lines\":{\"text\":\"a\x01b\"}}}\n",
		"bad base64":            `{"type":"match","data":{"lines":{"bytes":"*!"}}}` + "\n",
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			err := StreamEvents(context.Background(), strings.NewReader(stream), func(_ *RgEvent, parseErr error) error {
				return parseErr
			})
			if !errors.Is(err, ErrMalformedEvent) {
				t.Errorf("StreamEvents returned %v, want ErrMalformedEvent", err)
			}
		})
	}
}

// A callback that passes over a malformed event gets the events after
// it: the parser resumes at the next line.
func TestStreamEvents_ResumesAfterAMalformedLine(t *testing.T) {
	stream := "this is not json\n" + `{"type":"match","data":{"lines":{"text":"x\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}` + "\n"
	var errs, matches int
	err := StreamEvents(context.Background(), strings.NewReader(stream), func(ev *RgEvent, parseErr error) error {
		if parseErr != nil {
			errs++
			return nil
		}
		matches++
		return nil
	})
	if err != nil || errs != 1 || matches != 1 {
		t.Errorf("err %v, %d errors, %d matches; want nil, 1, 1", err, errs, matches)
	}
}

// referenceEvent is ripgrep's event decoded by encoding/json, the way
// rx read it before it bounded what it keeps.
type referenceEvent struct {
	Type string `json:"type"`
	Data struct {
		Lines struct {
			Text  *string `json:"text"`
			Bytes *string `json:"bytes"`
		} `json:"lines"`
		LineNumber     int   `json:"line_number"`
		AbsoluteOffset int64 `json:"absolute_offset"`
		Submatches     []struct {
			Match struct {
				Text  *string `json:"text"`
				Bytes *string `json:"bytes"`
			} `json:"match"`
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"submatches"`
	} `json:"data"`
}

func referenceText(text, bytes *string) string {
	if text != nil {
		return *text
	}
	if bytes != nil {
		return *bytes
	}
	return ""
}

// Under the limits, every field the parser reads from real ripgrep
// output equals what encoding/json reads: lines with quotes,
// backslashes, tabs, control characters, multibyte text, CRLF, bytes
// that are not UTF-8, a line longer than the read buffer, and several
// patterns with context around them.
func TestStreamEvents_ReadsRipgrepOutputAsEncodingJSONDoes(t *testing.T) {
	requireRipgrep(t)
	input := strings.Join([]string{
		"plain NEEDLE line",
		`quote " and backslash \ NEEDLE`,
		"tab\there NEEDLE and bell \x07 and esc \x1b",
		"café NEEDLE → 😀 ünïcödé",
		"crlf NEEDLE\r",
		"bad bytes \xff\xfe NEEDLE",
		"context line without the word",
		"long " + strings.Repeat("NEEDLE ab ", 2_000) + strings.Repeat("z", 100_000),
		"OTHER pattern here",
		"last NEEDLE without a break",
	}, "\n")
	cmd := exec.Command("rg", "--json", "--no-config", "-C", "1", "-e", "NEEDLE", "-e", "OTHER|é", "-")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rg: %v", err)
	}

	var want []referenceEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		var ev referenceEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("encoding/json: %v", err)
		}
		if ev.Type == "match" || ev.Type == "context" {
			want = append(want, ev)
		}
	}
	var got []*RgEvent
	err = StreamEvents(context.Background(), bytes.NewReader(out), func(ev *RgEvent, parseErr error) error {
		if parseErr != nil {
			return parseErr
		}
		if ev.Type == RgEventMatch || ev.Type == RgEventContext {
			got = append(got, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d line events, want %d", len(got), len(want))
	}
	for i, w := range want {
		lines, number, offset, subs := got[i].Context.linesOf()
		if got[i].Match != nil {
			lines, number, offset, subs = got[i].Match.Lines, got[i].Match.LineNumber, got[i].Match.AbsoluteOffset, got[i].Match.Submatches
			if got[i].Match.SubmatchesTruncated {
				t.Errorf("event %d: submatches marked truncated", i)
			}
		}
		where := fmt.Sprintf("event %d (%s, line %d)", i, w.Type, w.Data.LineNumber)
		if string(got[i].Type) != w.Type || number != w.Data.LineNumber || offset != w.Data.AbsoluteOffset {
			t.Errorf("%s: type %s line %d offset %d", where, got[i].Type, number, offset)
		}
		if lines.Text != referenceText(w.Data.Lines.Text, w.Data.Lines.Bytes) || lines.Truncated {
			t.Errorf("%s: text %q truncated %v", where, lines.Text, lines.Truncated)
		}
		if len(subs) != len(w.Data.Submatches) {
			t.Fatalf("%s: %d submatches, want %d", where, len(subs), len(w.Data.Submatches))
		}
		for j, ws := range w.Data.Submatches {
			if subs[j].Text() != referenceText(ws.Match.Text, ws.Match.Bytes) || subs[j].Start != ws.Start || subs[j].End != ws.End {
				t.Errorf("%s: submatch %d = %+v", where, j, subs[j])
			}
		}
	}
}

// linesOf reads a context event's line fields; nil gives zero values,
// so the caller can fall through to the match event.
func (c *RgContextData) linesOf() (RgText, int, int64, []RgSubmatch) {
	if c == nil {
		return RgText{}, 0, 0, nil
	}
	return c.Lines, c.LineNumber, c.AbsoluteOffset, c.Submatches
}
