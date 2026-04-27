package analyzer

import (
	"reflect"
	"testing"
)

// mkAnomaly is a terse constructor used throughout this test file to
// keep the table-driven cases readable. DetectorName is what Deduplicate
// keys on (see dedup.go); Category stays as a semantic bucket and
// happens to mirror the detector name here for readability.
func mkAnomaly(detector string, startOffset, endOffset int64) Anomaly {
	return Anomaly{
		// StartLine / EndLine aren't part of the dedup key but we set
		// them anyway so test assertions on the full Anomaly struct are
		// realistic. Using 1-based derived values keeps fixtures simple.
		StartLine:    startOffset/10 + 1,
		EndLine:      endOffset/10 + 1,
		StartOffset:  startOffset,
		EndOffset:    endOffset,
		Severity:     0.5,
		Category:     detector,
		DetectorName: detector,
		Description:  "test anomaly from " + detector,
	}
}

func TestDeduplicate_NilInputReturnsNil(t *testing.T) {
	// Nil input is a valid "no workers had anomalies" signal. Must
	// return nil (not an empty slice) so JSON encoders honor omitempty.
	got := Deduplicate(nil)
	if got != nil {
		t.Errorf("Deduplicate(nil) = %v, want nil", got)
	}
}

func TestDeduplicate_EmptyGroupsReturnsNil(t *testing.T) {
	// A slice of empty inner slices is the "workers ran but no anomalies"
	// case. Should still collapse to nil — nothing to encode.
	got := Deduplicate([][]Anomaly{nil, {}, nil})
	if got != nil {
		t.Errorf("Deduplicate(empty groups) = %v, want nil", got)
	}
}

func TestDeduplicate_SingleGroupPassesThrough(t *testing.T) {
	// With one group the function is essentially a copy — it must not
	// reorder, drop, or merge anything.
	in := [][]Anomaly{{
		mkAnomaly("traceback-python", 100, 200),
		mkAnomaly("long-line", 300, 400),
		mkAnomaly("repeat-identical", 500, 600),
	}}
	got := Deduplicate(in)
	if !reflect.DeepEqual(got, in[0]) {
		t.Errorf("single-group passthrough mismatch.\n got=%v\nwant=%v", got, in[0])
	}
}

func TestDeduplicate_ExactDuplicateAcrossGroupsCollapsesToOne(t *testing.T) {
	// The W-line overlap at chunk boundaries produces this exact case:
	// the anomaly whose range crosses the seam is detected by both
	// neighbors. The first occurrence wins.
	a := mkAnomaly("traceback-go", 1024, 2048)
	in := [][]Anomaly{
		{a}, // worker-0 saw it at the tail of its chunk+overlap
		{a}, // worker-1 saw it at the head of its chunk
	}
	got := Deduplicate(in)
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1 (collapsed duplicate)", len(got))
	}
	if !reflect.DeepEqual(got[0], a) {
		t.Errorf("collapsed anomaly mismatch.\n got=%v\nwant=%v", got[0], a)
	}
}

func TestDeduplicate_NonOverlappingStaysSeparate(t *testing.T) {
	// Distinct byte ranges must NOT collapse — they're different events
	// even when the detector name matches.
	in := [][]Anomaly{
		{mkAnomaly("long-line", 0, 100)},
		{mkAnomaly("long-line", 200, 300)},
		{mkAnomaly("long-line", 400, 500)},
	}
	got := Deduplicate(in)
	if len(got) != 3 {
		t.Fatalf("got %d anomalies, want 3 (non-overlapping)", len(got))
	}
}

func TestDeduplicate_SameOffsetsDifferentDetectorsStaySeparate(t *testing.T) {
	// If two detectors both flag the same byte range, that's by design
	// two distinct findings. The detector name is part of the key.
	in := [][]Anomaly{
		{mkAnomaly("traceback-python", 100, 200)},
		{mkAnomaly("long-line", 100, 200)},
	}
	got := Deduplicate(in)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2 (different detectors)", len(got))
	}
	// Order is group-order: python first, long-line second.
	if got[0].DetectorName != "traceback-python" || got[1].DetectorName != "long-line" {
		t.Errorf("order mismatch: got [%s %s], want [traceback-python long-line]",
			got[0].DetectorName, got[1].DetectorName)
	}
}

func TestDeduplicate_PartialOverlapKeepsBoth(t *testing.T) {
	// Ranges that share a boundary but aren't identical are NOT
	// duplicates — overlap is not a merge signal, exact match is.
	in := [][]Anomaly{
		{mkAnomaly("long-line", 0, 100)},
		{mkAnomaly("long-line", 50, 150)},
	}
	got := Deduplicate(in)
	if len(got) != 2 {
		t.Fatalf("got %d anomalies, want 2 (partial overlap, not duplicate)", len(got))
	}
}

func TestDeduplicate_PreservesOrderWithinGroup(t *testing.T) {
	// Within each group the relative order of survivors must match the
	// input. We construct a scenario where duplicates exist across
	// groups but the within-group order is non-trivial.
	in := [][]Anomaly{
		{
			mkAnomaly("a", 100, 200),
			mkAnomaly("b", 300, 400),
			mkAnomaly("c", 500, 600),
		},
		{
			mkAnomaly("a", 100, 200), // duplicate of group0[0]
			mkAnomaly("d", 700, 800), // new
		},
	}
	got := Deduplicate(in)
	want := []Anomaly{
		mkAnomaly("a", 100, 200),
		mkAnomaly("b", 300, 400),
		mkAnomaly("c", 500, 600),
		mkAnomaly("d", 700, 800),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order mismatch.\n got=%v\nwant=%v", got, want)
	}
}

func TestDeduplicate_FirstOccurrenceWins(t *testing.T) {
	// When duplicates disagree on non-key fields (Severity, Description,
	// line numbers), the first occurrence's values survive verbatim. We
	// do NOT attempt a field-level merge — a duplicate should be
	// bitwise-identical by construction, and if callers see divergence
	// the safest policy is "trust the first one".
	first := Anomaly{
		StartOffset: 100, EndOffset: 200,
		Category: "detector-x", DetectorName: "detector-x", Severity: 0.5, Description: "first",
	}
	second := Anomaly{
		StartOffset: 100, EndOffset: 200,
		Category: "detector-x", DetectorName: "detector-x", Severity: 0.9, Description: "second",
	}
	got := Deduplicate([][]Anomaly{{first}, {second}})
	if len(got) != 1 {
		t.Fatalf("got %d anomalies, want 1", len(got))
	}
	if got[0].Description != "first" || got[0].Severity != 0.5 {
		t.Errorf("first-wins violated: got %+v, want %+v", got[0], first)
	}
}

func TestDeduplicate_DeterministicOutput(t *testing.T) {
	// Running Deduplicate twice on the same input must produce the same
	// output. Relies on map iteration NOT influencing output order —
	// our implementation builds a slice and uses the map only for
	// membership, so this is by construction. The test guards against
	// a future refactor that (for example) returns map-keyed output.
	in := [][]Anomaly{
		{
			mkAnomaly("a", 100, 200),
			mkAnomaly("b", 300, 400),
		},
		{
			mkAnomaly("a", 100, 200),
			mkAnomaly("c", 500, 600),
		},
	}
	a := Deduplicate(in)
	b := Deduplicate(in)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("non-deterministic output:\n a=%v\n b=%v", a, b)
	}
}

// BenchmarkDeduplicate_1000x10 exercises the typical shape: 10 workers,
// each emitting ~1000 anomalies with some small percentage duplicated
// at the seams. Flags regressions in the allocation profile.
func BenchmarkDeduplicate_1000x10(b *testing.B) {
	const groups = 10
	const perGroup = 1000
	in := make([][]Anomaly, groups)
	for g := 0; g < groups; g++ {
		slice := make([]Anomaly, perGroup)
		for i := 0; i < perGroup; i++ {
			// Offsets chosen so ~5% of anomalies collide across groups
			// (the index*10 term is shared across every other group).
			offset := int64(g*perGroup + i)
			if i%20 == 0 {
				// Duplicate an anomaly that also appears in group (g+1)%groups.
				offset = int64(((g + 1) % groups) * perGroup)
			}
			slice[i] = mkAnomaly("bench", offset*10, offset*10+5)
		}
		in[g] = slice
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Deduplicate(in)
	}
}
