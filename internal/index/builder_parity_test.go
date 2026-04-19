package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBuild_PythonParity_ReadmeFixture is the gold-standard parity test.
//
// The golden data in testdata/readme-py.golden.json was produced by
// running rx-python's build_index on rx-python/README.md with
// step_bytes=256 and analyze=True. If this test fails, the Go builder
// has drifted from the Python reference.
//
// How to regenerate:
//
//	cd rx-python
//	./.venv/bin/python -c 'from rx.unified_index import build_index; \
//	    import json, dataclasses as dc; \
//	    r = build_index("README.md", 256); \
//	    print(json.dumps({"line_index": r.line_index, \
//	        "line_count": r.line_count, "empty_line_count": r.empty_line_count, \
//	        "line_length_max": r.line_length_max, "line_ending": r.line_ending}, \
//	        indent=2))' > ../rx-go/internal/index/testdata/readme-py.golden.json
//
// Then copy rx-python/README.md to internal/index/testdata/readme-source.md
// so the Go test has the same input bytes.
func TestBuild_PythonParity_ReadmeFixture(t *testing.T) {
	source := filepath.Join("testdata", "readme-source.md")
	golden := filepath.Join("testdata", "readme-py.golden.json")

	if _, err := os.Stat(source); err != nil {
		t.Skipf("skipping parity test: %s not present (%v)", source, err)
	}
	if _, err := os.Stat(golden); err != nil {
		t.Skipf("skipping parity test: %s not present (%v)", golden, err)
	}

	goldenBytes, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want struct {
		LineIndex      [][]int64 `json:"line_index"`
		LineCount      int64     `json:"line_count"`
		EmptyLineCount int64     `json:"empty_line_count"`
		LineLengthMax  int64     `json:"line_length_max"`
		LineEnding     string    `json:"line_ending"`
	}
	if err := json.Unmarshal(goldenBytes, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	// Run the Go builder with the SAME step bytes the Python golden used.
	idx, err := Build(source, BuildOptions{StepBytes: 256, Analyze: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if idx.LineCount == nil || *idx.LineCount != want.LineCount {
		t.Errorf("LineCount: got %v, want %d", idx.LineCount, want.LineCount)
	}
	if idx.EmptyLineCount == nil || *idx.EmptyLineCount != want.EmptyLineCount {
		t.Errorf("EmptyLineCount: got %v, want %d", idx.EmptyLineCount, want.EmptyLineCount)
	}
	if idx.LineLengthMax == nil || *idx.LineLengthMax != want.LineLengthMax {
		t.Errorf("LineLengthMax: got %v, want %d", idx.LineLengthMax, want.LineLengthMax)
	}
	if idx.LineEnding == nil || *idx.LineEnding != want.LineEnding {
		t.Errorf("LineEnding: got %v, want %q", idx.LineEnding, want.LineEnding)
	}

	if len(idx.LineIndex) != len(want.LineIndex) {
		t.Fatalf("LineIndex length: got %d, want %d (Go: %+v)\n(Python: %+v)",
			len(idx.LineIndex), len(want.LineIndex), idx.LineIndex, want.LineIndex)
	}
	for i, entry := range want.LineIndex {
		if len(entry) < 2 {
			t.Fatalf("golden entry %d malformed: %v", i, entry)
		}
		wantLine := entry[0]
		wantOffset := entry[1]
		gotLine := idx.LineIndex[i].LineNumber
		gotOffset := idx.LineIndex[i].ByteOffset
		if gotLine != wantLine || gotOffset != wantOffset {
			t.Errorf("checkpoint %d: got {%d, %d}, want {%d, %d}",
				i, gotLine, gotOffset, wantLine, wantOffset)
		}
	}
}
