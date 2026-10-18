package timestamps

import "testing"

// A UTF-8 byte-order mark at the start of a line is not part of the
// line's text: an anchored format finds its timestamp at column 0 right
// after the mark (or at column 1 after a bracket), a windowed format
// as it would without the mark, and the span still counts the mark's
// three bytes, since offsets in a line do.
func TestOwn_SkipsAByteOrderMark(t *testing.T) {
	ts := wall(utcMs(2026, 10, 6, 12, 34, 56, 0))
	runOwnCases(t, []ownCase{
		{"anchored after the mark", isoAnchored, "\xEF\xBB\xBF2026-10-06 12:34:56 x", ts, true},
		{"anchored after the mark and a bracket", isoAnchored, "\xEF\xBB\xBF[2026-10-06 12:34:56] x", ts, true},
		{"anchored ignores a space after the mark", isoAnchored, "\xEF\xBB\xBF 2026-10-06 12:34:56 x", Stamp{}, false},
		{"windowed after the mark", isoWindowed, "\xEF\xBB\xBFabcd 2026-10-06 12:34:56 x", ts, true},
		{"a mark alone is no timestamp", isoAnchored, "\xEF\xBB\xBF", Stamp{}, false},
	}, mtime2025)

	p := mustParser(t, isoAnchored, mtime2025)
	_, span, ok := p.Locate([]byte("\xEF\xBB\xBF2026-10-06 12:34:56 x"))
	if !ok || span.Start != 3 || span.End != 3+len("2026-10-06 12:34:56") {
		t.Errorf("Locate after a mark: span %+v, %t; want 3-22", span, ok)
	}
	if text, ok := p.Text([]byte("\xEF\xBB\xBF2026-10-06 12:34:56 x")); !ok || text != "2026-10-06 12:34:56" {
		t.Errorf("Text after a mark = %q, %t", text, ok)
	}
}

// Detection reads the first line through the mark as well: the three
// anchored lines an anchored format needs may include a marked line 1.
func TestDetect_SkipsAByteOrderMarkOnTheFirstLine(t *testing.T) {
	f, ok := Detect([]byte("\xEF\xBB\xBF2026-10-06 12:34:56 INFO start\n2026-10-06 12:34:57 INFO next\n2026-10-06 12:34:58 INFO last\n"))
	if !ok || f.Family != FamilyISO || !f.Anchored {
		t.Fatalf("Detect = %+v, %t; want an anchored ISO format", f, ok)
	}
	// Without the mark skipped, line 1 would not count as anchored and
	// two lines would not qualify: the marked line is the third.
	f, ok = Detect([]byte("x 2026-10-06 12:34:56 INFO start\n2026-10-06 12:34:57 INFO next\n2026-10-06 12:34:58 INFO last\n"))
	if !ok || f.Anchored {
		t.Fatalf("control: Detect = %+v, %t; want a windowed format when line 1 is not anchored", f, ok)
	}
}
