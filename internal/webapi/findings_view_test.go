package webapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

func TestParseLineRange(t *testing.T) {
	cases := []struct {
		value   string
		want    *linesFilter
		wantErr bool
	}{
		{value: "", want: nil},
		{value: "5", want: &linesFilter{5, 5}},
		{value: "3-7", want: &linesFilter{3, 7}},
		{value: "7-7", want: &linesFilter{7, 7}},
		{value: "0-2", wantErr: true},
		{value: "7-3", wantErr: true},
		{value: "a-b", wantErr: true},
		{value: "1-", wantErr: true},
		{value: "-3", wantErr: true},
		{value: "1-2-3", wantErr: true},
		{value: "99999999999999999999", wantErr: true},
		{value: strings.Repeat("1", 100), wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseLineRange(tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err %v, want error %v", tc.value, err, tc.wantErr)
			continue
		}
		if tc.want == nil && got != nil || tc.want != nil && (got == nil || *got != *tc.want) {
			t.Errorf("%q: %+v, want %+v", tc.value, got, tc.want)
		}
	}
}

// The lines parameter comes from a request: any text gives a range with
// 1 <= first <= last, or an error that quotes at most a bounded part of
// it, and never a panic.
func FuzzParseLineRange(f *testing.F) {
	for _, seed := range []string{"", "1", "3-7", "0-1", "9-2", "x", "1-2-3", "-", strings.Repeat("9", 70)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := parseLineRange(value)
		switch {
		case err != nil:
			if len(err.Error()) > 4*maxEchoedValueBytes+200 {
				t.Fatalf("error of %d bytes for a value of %d", len(err.Error()), len(value))
			}
		case got == nil:
			if value != "" {
				t.Fatalf("%q: no range and no error", value)
			}
		case got.First < 1 || got.Last < got.First:
			t.Fatalf("%q: range %+v", value, got)
		}
	})
}

// viewCatalogue loads two signatures of two groups for the filter tests.
func viewCatalogue(t *testing.T) *signatures.Catalogue {
	t.Helper()
	dir := t.TempDir()
	body := `signatures:
  - id: err-line
    name: Error line
    group: severity
    description: A line that says ERROR.
    regex: 'ERROR'
  - id: py-trace
    name: Python traceback
    group: traceback
    description: A traceback.
    block:
      start: '^Traceback'
      continue: '^\s'
`
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{dir}})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return cat
}

// Review focus: each filter keeps what it names, they combine, and
// before_line lists the nearest finding first.
func TestFindingsFilter_EachFilterAndTheirOrder(t *testing.T) {
	cat := viewCatalogue(t)
	all := []rxtypes.Finding{
		{Signature: "err-line", StartLine: 2, EndLine: 2, Excerpt: "ERROR disk full"},
		{Signature: "py-trace", StartLine: 4, EndLine: 9, Excerpt: "Traceback (most recent call last):"},
		{Signature: "err-line", StartLine: 12, EndLine: 12, Excerpt: "ERROR Timeout"},
		{Signature: "err-line", StartLine: 20, EndLine: 20, Excerpt: "ERROR again"},
	}
	lines := func(found []rxtypes.Finding) []int64 {
		out := []int64{}
		for _, f := range found {
			out = append(out, f.StartLine)
		}
		return out
	}
	cases := []struct {
		name   string
		filter findingsFilter
		want   []int64
	}{
		{"none", findingsFilter{}, []int64{2, 4, 12, 20}},
		{"signature", findingsFilter{signatures: []string{"py-trace"}}, []int64{4}},
		{"group", findingsFilter{groups: []string{"severity"}}, []int64{2, 12, 20}},
		{"unknown group", findingsFilter{groups: []string{"nope"}}, []int64{}},
		{"q in an excerpt, any case", findingsFilter{query: "timeout"}, []int64{12}},
		{"q in a signature name", findingsFilter{query: "python"}, []int64{4}},
		{"lines overlap", findingsFilter{lines: &linesFilter{8, 12}}, []int64{4, 12}},
		{"after line", findingsFilter{afterLine: 4}, []int64{12, 20}},
		{"before line, nearest first", findingsFilter{beforeLine: 20}, []int64{12, 4, 2}},
		{"after and before", findingsFilter{afterLine: 2, beforeLine: 20}, []int64{12, 4}},
		{"group and after", findingsFilter{groups: []string{"severity"}, afterLine: 2}, []int64{12, 20}},
	}
	for _, tc := range cases {
		if got := lines(tc.filter.apply(all, cat)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: lines %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := lines(pageOf(findingsFilter{}.apply(all, cat), 1, 2)); !slices.Equal(got, []int64{4, 12}) {
		t.Errorf("page 1+2: %v", got)
	}
	if got := pageOf(all, 9, 2); got == nil || len(got) != 0 {
		t.Errorf("page past the end: %v", got)
	}
}

// Ticks put each finding with a time in its column over the file's
// range, count per signature, keep the first line, and leave out a
// finding without a time or outside the range; no range, no columns.
func TestTicksOf_ColumnsOverTheTimeRange(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	found := []rxtypes.Finding{
		{Signature: "err-line", StartLine: 30, TimeMS: ms(1000)},
		{Signature: "err-line", StartLine: 10, TimeMS: ms(1009)},
		{Signature: "py-trace", StartLine: 12, TimeMS: ms(1005)},
		{Signature: "err-line", StartLine: 50, TimeMS: ms(1999)},
		{Signature: "err-line", StartLine: 60, TimeMS: ms(2000)},
		{Signature: "err-line", StartLine: 70, TimeMS: nil},
		{Signature: "err-line", StartLine: 80, TimeMS: ms(999)},
		{Signature: "err-line", StartLine: 90, TimeMS: ms(2001)},
	}
	ticks := ticksOf(found, 100, ms(1000), ms(1999))
	want := []rxtypes.TickColumn{
		{Column: 0, Signatures: map[string]rxtypes.TickCount{"err-line": {Count: 2, FirstLine: 10}, "py-trace": {Count: 1, FirstLine: 12}}},
		{Column: 99, Signatures: map[string]rxtypes.TickCount{"err-line": {Count: 1, FirstLine: 50}}},
	}
	if ticks.Width != 100 || *ticks.StartMS != 1000 || *ticks.EndMS != 1999 || len(ticks.Columns) != len(want) {
		t.Fatalf("ticks %+v", ticks)
	}
	for i := range want {
		got := ticks.Columns[i]
		if got.Column != want[i].Column || len(got.Signatures) != len(want[i].Signatures) {
			t.Fatalf("column %d: %+v, want %+v", i, got, want[i])
		}
		for id, c := range want[i].Signatures {
			if got.Signatures[id] != c {
				t.Errorf("column %d %s: %+v, want %+v", got.Column, id, got.Signatures[id], c)
			}
		}
	}
	// 1000..1015 in 4 columns of 4 ms: 1000, 1005 and 1009 fall in
	// columns 0, 1 and 2.
	if middle := ticksOf(found[:3], 4, ms(1000), ms(1015)); len(middle.Columns) != 3 || middle.Columns[1].Column != 1 ||
		middle.Columns[1].Signatures["py-trace"].Count != 1 || middle.Columns[2].Column != 2 {
		t.Errorf("1000..1015 in 4 columns: %+v", middle.Columns)
	}
	if one := ticksOf(found, 10, ms(1000), ms(1000)); len(one.Columns) != 1 || one.Columns[0].Column != 0 ||
		one.Columns[0].Signatures["err-line"].Count != 1 {
		t.Errorf("a range of one instant: %+v", one.Columns)
	}
	for _, none := range []*rxtypes.FindingsTicks{ticksOf(found, 10, nil, ms(5)), ticksOf(found, 10, ms(5), nil), ticksOf(found, 10, ms(9), ms(5))} {
		if none.Columns == nil || len(none.Columns) != 0 {
			t.Errorf("no range: columns %+v", none.Columns)
		}
	}
}
