package samples

import (
	"testing"
)

// A position counted from the end that reaches before the first line
// names no line: it keeps the key it was asked by, answers -1 and a
// null sample, as a line past the end does. A range that starts before
// the first line keeps its key and holds the lines the file has. A
// negative offset before the first byte answers the same way. The rule
// is the same in every storage, with an index or without one.
func TestResolve_PositionsBeforeTheStartAreMissing(t *testing.T) {
	const total = 30
	plain, gz := writeGzipFixture(t, total)
	zst, zstLoader, _ := makeIndexedSeekable(t, total, 512)
	n := func(v int64) *int64 { return &v }

	for _, tc := range []struct {
		name   string
		path   string
		loader IndexLoader
	}{
		{"plain", plain, NoIndex},
		{"gzip", gz, NoIndex},
		{"seekable through its frame table", zst, zstLoader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := Resolve(t.Context(), Request{
				Path: tc.path, IndexLoader: tc.loader, BeforeContext: 1, AfterContext: 1,
				Lines: []OffsetOrRange{{Start: -31}, {Start: -100}, {Start: -40, End: n(-35)}, {Start: -35, End: n(-28)}, {Start: -1}},
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			for _, key := range []string{"-31", "-100", "-40--35"} {
				if v, ok := resp.Lines[key]; !ok || v != -1 {
					t.Errorf("lines[%q] = %d, %v; want -1 under the key as asked (lines %v)", key, v, ok, resp.Lines)
				}
				if s, ok := resp.Samples[key]; !ok || s != nil {
					t.Errorf("samples[%q] = %v, %v; want null", key, s, ok)
				}
			}
			if got := resp.Samples["-35--28"]; len(got) != 3 || resp.Lines["-35--28"] != -1 {
				t.Errorf("samples[-35--28] = %v (lines %v), want lines 1-3 of the file under the key as asked", got, resp.Lines["-35--28"])
			}
			if got := resp.Samples["30"]; len(got) != 2 {
				t.Errorf("samples[30] = %v, want line 30 with its one line of context before it", got)
			}
			if _, clamped := resp.Lines["1"]; clamped {
				t.Errorf("a position before the start was clamped to line 1: %v", resp.Lines)
			}
		})
	}

	resp, err := Resolve(t.Context(), Request{Path: plain, IndexLoader: NoIndex, Offsets: []OffsetOrRange{{Start: -9999}, {Start: -5}}})
	if err != nil {
		t.Fatalf("Resolve(offsets): %v", err)
	}
	if v, ok := resp.Offsets["-9999"]; !ok || v != -1 || resp.Samples["-9999"] != nil {
		t.Errorf("offsets[-9999] = %d, %v, samples %v; want -1 and null under the key as asked", v, ok, resp.Samples["-9999"])
	}
	if _, clamped := resp.Offsets["0"]; clamped {
		t.Errorf("an offset before the start was clamped to 0: %v", resp.Offsets)
	}
	if len(resp.Offsets) != 2 {
		t.Errorf("offsets = %v, want the missing one and -5 resolved", resp.Offsets)
	}
}

// A request whose every position is before the start still answers,
// with nothing read.
func TestResolve_EveryPositionBeforeTheStart(t *testing.T) {
	plain, _ := writeGzipFixture(t, 30)
	resp, err := Resolve(t.Context(), Request{Path: plain, IndexLoader: NoIndex, Offsets: []OffsetOrRange{{Start: -9999}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Offsets["-9999"] != -1 {
		t.Errorf("offsets = %v, want {-9999: -1}", resp.Offsets)
	}
}
