package samples

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// writeFixture writes a deterministic 20-line fixture where each line
// is "line NNN\n" (9 bytes). Line N starts at byte offset (N-1)*9.
func writeFixture(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "fixture.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %03d\n", i)
	}
	if err := os.WriteFile(f, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return f
}

// TestResolve_LinesMode_SingleWithContext covers the most common path
// (single line, context window). Verifies Stage 9 Round 2 R1-B5: the
// lines[key] byte offset must be the offset of the REQUESTED line,
// not the context window's first line.
func TestResolve_LinesMode_SingleWithContext(t *testing.T) {
	f := writeFixture(t)
	end5 := int64(0) // unused
	_ = end5
	req := Request{
		Path:          f,
		Lines:         []OffsetOrRange{{Start: 10}},
		BeforeContext: 3,
		AfterContext:  3,
		IndexLoader:   NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Lines["10"] != int64((10-1)*9) {
		t.Errorf("Lines[\"10\"] = %d, want %d", resp.Lines["10"], (10-1)*9)
	}
	sample := resp.Samples["10"]
	if len(sample) != 7 {
		t.Errorf("sample len = %d, want 7 (3 before + 1 target + 3 after)", len(sample))
	}
	if sample[3] != "line 010" {
		t.Errorf("middle sample = %q, want 'line 010'", sample[3])
	}
}

// TestResolve_LinesMode_RangeEmitsNegativeOneOffset — Python parity:
// line-mode ranges use sentinel -1 for the offset (Python:
// "byte offset is not meaningful for ranges").
func TestResolve_LinesMode_RangeEmitsNegativeOneOffset(t *testing.T) {
	f := writeFixture(t)
	end := int64(8)
	req := Request{
		Path:        f,
		Lines:       []OffsetOrRange{{Start: 5, End: &end}},
		IndexLoader: NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Lines["5-8"] != -1 {
		t.Errorf("Lines[\"5-8\"] = %d, want -1", resp.Lines["5-8"])
	}
	if len(resp.Samples["5-8"]) != 4 {
		t.Errorf("sample len = %d, want 4", len(resp.Samples["5-8"]))
	}
}

// TestResolve_OffsetsMode_SingleByte covers Stage 9 Round 2 R1-B4: byte
// offset dispatch must resolve to the line containing the offset (not
// treat the value as a line number). Line 10 starts at byte 81 — an
// offset of 85 falls inside line 10.
func TestResolve_OffsetsMode_SingleByte(t *testing.T) {
	f := writeFixture(t)
	req := Request{
		Path:          f,
		Offsets:       []OffsetOrRange{{Start: 85}}, // byte in line 10
		BeforeContext: 0,
		AfterContext:  0,
		IndexLoader:   NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Offsets["85"] != 10 {
		t.Errorf("Offsets[\"85\"] = %d, want 10 (line containing byte 85)",
			resp.Offsets["85"])
	}
	sample := resp.Samples["85"]
	if len(sample) != 1 || sample[0] != "line 010" {
		t.Errorf("sample = %v, want ['line 010']", sample)
	}
}

// TestResolve_OffsetsMode_ByteRange covers the range branch of byte
// offset mode. Range [10, 50] overlaps lines 2-6 (10/9=1.1 → starts in
// line 2; 50/9=5.5 → ends in line 6). Multi-line span.
func TestResolve_OffsetsMode_ByteRange(t *testing.T) {
	f := writeFixture(t)
	end := int64(50)
	req := Request{
		Path:        f,
		Offsets:     []OffsetOrRange{{Start: 10, End: &end}},
		IndexLoader: NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.Offsets["10-50"] != 2 {
		t.Errorf("Offsets[\"10-50\"] = %d, want 2 (start line)", resp.Offsets["10-50"])
	}
	sample := resp.Samples["10-50"]
	if len(sample) < 5 {
		t.Errorf("sample len = %d, want >= 5", len(sample))
	}
}

// TestResolve_MultiRange covers the Stage 9 Round 2 R1-B4 user design:
// multiple ranges in a single call. Every key must appear in the
// response with its own sample slice.
func TestResolve_MultiRange(t *testing.T) {
	f := writeFixture(t)
	end1 := int64(5)
	end2 := int64(15)
	req := Request{
		Path: f,
		Lines: []OffsetOrRange{
			{Start: 2, End: &end1},
			{Start: 10, End: &end2},
			{Start: 20}, // single with context
		},
		BeforeContext: 1,
		AfterContext:  1,
		IndexLoader:   NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, key := range []string{"2-5", "10-15", "20"} {
		if _, ok := resp.Samples[key]; !ok {
			t.Errorf("missing key %q in Samples: %v", key, resp.Samples)
		}
	}
	if len(resp.Samples["2-5"]) != 4 {
		t.Errorf("2-5 sample len = %d, want 4", len(resp.Samples["2-5"]))
	}
	if len(resp.Samples["10-15"]) != 6 {
		t.Errorf("10-15 sample len = %d, want 6", len(resp.Samples["10-15"]))
	}
}

// TestResolve_NegativeSingleLine — Python parity: `lines=-1` means
// "last line" (and -2 means "second to last", etc.). Requires total
// line count, which uses the provided IndexLoader when possible. The
// resolved positive line number is used as the map key (Python
// samples.py reassigns `start` before stringifying).
func TestResolve_NegativeSingleLine(t *testing.T) {
	f := writeFixture(t)
	req := Request{
		Path:          f,
		Lines:         []OffsetOrRange{{Start: -1}},
		BeforeContext: 0,
		AfterContext:  0,
		IndexLoader:   NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Key is the resolved positive line number (20, the last line).
	sample := resp.Samples["20"]
	if len(sample) != 1 || sample[0] != "line 020" {
		t.Errorf("negative line -1: sample = %v, want ['line 020']", sample)
	}
	// Offset of line 20 is (20-1)*9 = 171.
	if resp.Lines["20"] != 171 {
		t.Errorf("Lines[\"20\"] = %d, want 171", resp.Lines["20"])
	}
}

// TestResolve_IndexAwareSeek stubs an IndexLoader returning a
// synthetic checkpoint at line 15 → offset 126. The resolver must
// consult the index and seek directly to byte 126 rather than scanning
// from 0. We don't verify the seek-delta directly (no instrumentation),
// but we verify the output is IDENTICAL to the no-index path.
func TestResolve_IndexAwareSeek(t *testing.T) {
	f := writeFixture(t)
	// Line 15 starts at offset (15-1)*9 = 126.
	ix := &rxtypes.UnifiedFileIndex{
		LineIndex: []rxtypes.LineIndexEntry{
			{LineNumber: 1, ByteOffset: 0},
			{LineNumber: 15, ByteOffset: 126},
		},
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) {
		return ix, nil
	}
	reqWithIdx := Request{
		Path:          f,
		Lines:         []OffsetOrRange{{Start: 17}},
		BeforeContext: 1,
		AfterContext:  1,
		IndexLoader:   loader,
	}
	reqNoIdx := reqWithIdx
	reqNoIdx.IndexLoader = NoIndex

	respA, err := Resolve(reqWithIdx)
	if err != nil {
		t.Fatalf("with index: %v", err)
	}
	respB, err := Resolve(reqNoIdx)
	if err != nil {
		t.Fatalf("no index: %v", err)
	}
	// Sample contents must be identical.
	if !sliceEqual(respA.Samples["17"], respB.Samples["17"]) {
		t.Errorf("samples differ — index-aware seek broke line indexing\n  with idx: %v\n  no idx:   %v",
			respA.Samples["17"], respB.Samples["17"])
	}
	// Line offsets must match (offset of line 17 is 144).
	if respA.Lines["17"] != respB.Lines["17"] {
		t.Errorf("offsets differ: with=%d no=%d", respA.Lines["17"], respB.Lines["17"])
	}
	if respA.Lines["17"] != int64((17-1)*9) {
		t.Errorf("Lines[\"17\"] = %d, want 144", respA.Lines["17"])
	}
}

// TestResolve_NegativeByteOffset — Python parity: `-offset` resolves
// against file size. -1 means "last byte"; the map key is the resolved
// positive offset (179), not the user's signed input. This matches
// Python's samples.py behavior where `start` is reassigned before
// being stringified for the key.
func TestResolve_NegativeByteOffset(t *testing.T) {
	f := writeFixture(t)
	// File is 20 lines × 9 bytes = 180 bytes. -1 → offset 179 → line 20.
	req := Request{
		Path:          f,
		Offsets:       []OffsetOrRange{{Start: -1}},
		BeforeContext: 0,
		AfterContext:  0,
		IndexLoader:   NoIndex,
	}
	resp, err := Resolve(req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Key is the resolved positive offset (179 = file_size - 1).
	if resp.Offsets["179"] != 20 {
		t.Errorf("Offsets[\"179\"] = %d, want 20 (line containing last byte)",
			resp.Offsets["179"])
	}
}

// sliceEqual is the helper for comparing two []string (stdlib has
// slices.Equal at Go 1.21+ but this avoids the import).
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
