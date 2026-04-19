package rxtypes

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// intPtr / strPtr / int64Ptr / float64Ptr — tiny helpers for populating
// pointer fields in test fixtures without cluttering table-driven cases.
func intPtr(v int) *int             { return &v }
func strPtr(v string) *string       { return &v }
func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }

func TestErrorResponse_JSON(t *testing.T) {
	t.Parallel()
	e := ErrorResponse{Detail: "not found"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"detail":"not found"}`
	if string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
}

func TestSubmatch_JSON(t *testing.T) {
	t.Parallel()
	s := Submatch{Text: "err", Start: 10, End: 13}
	data, _ := json.Marshal(s)
	want := `{"text":"err","start":10,"end":13}`
	if string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
	var decoded Submatch
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != s {
		t.Errorf("round-trip mismatch: got %+v want %+v", decoded, s)
	}
}

func TestMatch_JSON_Null_LineText(t *testing.T) {
	t.Parallel()
	// When LineText is nil we want `"line_text": null` — matching Python
	// Pydantic's default serialization, not `omitempty`.
	m := Match{
		Pattern:            "p1",
		File:               "f1",
		Offset:             100,
		RelativeLineNumber: intPtr(5),
		AbsoluteLineNumber: 5,
		LineText:           nil,
		Submatches:         []Submatch{},
	}
	data, _ := json.Marshal(m)
	if !strings.Contains(string(data), `"line_text":null`) {
		t.Errorf("expected `line_text:null` in %s", data)
	}
	// Empty submatches list should serialize as [], not be omitted.
	if !strings.Contains(string(data), `"submatches":[]`) {
		t.Errorf("expected `submatches:[]` in %s", data)
	}
}

func TestTraceResponse_FieldOrder(t *testing.T) {
	t.Parallel()
	// Struct field order determines JSON key order. This test pins the
	// order to match Python's Pydantic emission — if someone reorders
	// struct fields they must update both this test and the Stage 9
	// parity fixture.
	//
	// Stage 9 Round 2 S2 user decision: schema-documented fields must
	// emit explicit null when unset. omitempty is only acceptable for
	// fields that are "extensions" NOT part of the advertised schema.
	// Python emits file_chunks/context_lines/before_context/after_context/
	// cli_command as null — Go must match.
	resp := TraceResponse{
		RequestID:    "01936c8e-7b2a-7000-8000-000000000001",
		Path:         []string{"/tmp"},
		Time:         0.123,
		Patterns:     map[string]string{"p1": "error"},
		Files:        map[string]string{"f1": "/tmp/a.log"},
		Matches:      []Match{},
		ScannedFiles: []string{},
		SkippedFiles: []string{},
		MaxResults:   nil,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Extract keys in order from the top-level JSON object.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Every documented schema field MUST appear in the JSON, even when
	// unset — matches Python Pydantic emission (see round-1 triage S2).
	for _, required := range []string{
		"request_id", "path", "time", "patterns", "files",
		"matches", "scanned_files", "skipped_files", "max_results",
		"file_chunks", "context_lines", "before_context", "after_context",
		"cli_command",
	} {
		if _, present := raw[required]; !present {
			t.Errorf("required field %q missing in JSON: %s", required, data)
		}
	}
	// Nullable fields that are unset must be explicit null — NOT absent,
	// NOT empty object, NOT empty string.
	for _, nullable := range []string{
		"max_results", "file_chunks", "context_lines",
		"before_context", "after_context",
	} {
		if string(raw[nullable]) != "null" {
			t.Errorf("field %q should emit null when unset, got %s", nullable, raw[nullable])
		}
	}
	// cli_command is a string field — Python emits it as null. Go's
	// string type cannot emit null natively, so the field is typed as
	// *string.
	if string(raw["cli_command"]) != "null" {
		t.Errorf("cli_command should emit null when unset, got %s", raw["cli_command"])
	}
}

func TestTraceResponse_KeyOrdering(t *testing.T) {
	t.Parallel()
	resp := TraceResponse{
		RequestID: "id",
		Path:      []string{"/tmp"},
		Patterns:  map[string]string{},
		Files:     map[string]string{},
	}
	data, _ := json.Marshal(resp)
	// Python emits: request_id, path, time, patterns, files, matches,
	// scanned_files, skipped_files, max_results, ...
	// Go's encoding/json walks struct fields in declaration order, so
	// we just need to assert `request_id` is FIRST and `path` is SECOND.
	s := string(data)
	i0 := strings.Index(s, `"request_id"`)
	i1 := strings.Index(s, `"path"`)
	i2 := strings.Index(s, `"time"`)
	if i0 < 0 || i1 < 0 || i2 < 0 {
		t.Fatalf("missing keys in %s", s)
	}
	if i0 >= i1 || i1 >= i2 {
		t.Errorf("key order wrong; got request_id@%d, path@%d, time@%d", i0, i1, i2)
	}
}

func TestSamplesResponse_JSON(t *testing.T) {
	t.Parallel()
	r := SamplesResponse{
		Path:              "/tmp/a.log",
		Offsets:           map[string]int64{"123": 1},
		Lines:             map[string]int64{},
		BeforeContext:     3,
		AfterContext:      3,
		Samples:           map[string][]string{"123": {"line before", "match", "line after"}},
		IsCompressed:      false,
		CompressionFormat: nil,
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Stage 9 Round 2 S2: schema-documented fields must emit null when
	// unset. Python emits both compression_format and cli_command as
	// null on a default SamplesResponse; Go must match.
	if !strings.Contains(string(data), `"compression_format":null`) {
		t.Errorf("expected compression_format:null in %s", data)
	}
	if !strings.Contains(string(data), `"cli_command":null`) {
		t.Errorf("expected cli_command:null in %s", data)
	}
	// Round-trip.
	var decoded SamplesResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.Samples, r.Samples) {
		t.Errorf("samples mismatch: %+v vs %+v", decoded.Samples, r.Samples)
	}
}

func TestUnifiedFileIndex_NullableSchemaFields(t *testing.T) {
	t.Parallel()
	// Stage 9 Round 2 S2: frames / anomalies / anomaly_summary are
	// documented schema fields on Python's UnifiedFileIndex. They must
	// emit null when unset (NOT be omitted). Python emits all three as
	// null on a default text-file index.
	src := UnifiedFileIndex{
		Version:           2,
		SourcePath:        "/tmp/x.log",
		SourceModifiedAt:  "2026-04-17T12:34:56.123456",
		SourceSizeBytes:   1024,
		CreatedAt:         "2026-04-17T12:34:56.123456",
		BuildTimeSeconds:  0.5,
		FileType:          FileTypeText,
		IsText:            true,
		LineIndex:         []LineIndexEntry{{1, 0}},
		AnalysisPerformed: false,
		// Frames / Anomalies / AnomalySummary deliberately unset — these
		// are schema fields documented in rx-python/src/rx/models.py and
		// must appear as null in the output.
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, frag := range []string{
		`"frames":null`,
		`"anomalies":null`,
		`"anomaly_summary":null`,
	} {
		if !strings.Contains(string(data), frag) {
			t.Errorf("expected %s in JSON, got %s", frag, data)
		}
	}
}

func TestUnifiedFileIndex_MinimalRoundTrip(t *testing.T) {
	t.Parallel()
	// Minimal required fields; ensure they serialize without panic and
	// the round trip preserves the schema.
	src := UnifiedFileIndex{
		Version:           2,
		SourcePath:        "/tmp/x.log",
		SourceModifiedAt:  "2026-04-17T12:34:56.123456",
		SourceSizeBytes:   1024,
		CreatedAt:         "2026-04-17T12:34:56.123456",
		BuildTimeSeconds:  0.5,
		FileType:          FileTypeText,
		IsText:            true,
		LineIndex:         []LineIndexEntry{{1, 0}, {100, 4096}},
		AnalysisPerformed: false,
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded UnifiedFileIndex
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Version != src.Version {
		t.Errorf("version mismatch: got %d want %d", decoded.Version, src.Version)
	}
	if decoded.FileType != src.FileType {
		t.Errorf("file type mismatch: got %s want %s", decoded.FileType, src.FileType)
	}
	if len(decoded.LineIndex) != len(src.LineIndex) {
		t.Errorf("line index length: got %d want %d", len(decoded.LineIndex), len(src.LineIndex))
	}
}

func TestTaskResponse_JSON(t *testing.T) {
	t.Parallel()
	r := TaskResponse{
		TaskID:    "01234567-89ab-cdef",
		Status:    "queued",
		Message:   "task accepted",
		Path:      "/tmp/a.log",
		StartedAt: strPtr("2026-04-17T12:34:56.123456"),
	}
	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `"task_id":"01234567-89ab-cdef"`) {
		t.Errorf("unexpected JSON: %s", data)
	}
	if !strings.Contains(string(data), `"started_at":"2026-04-17T12:34:56.123456"`) {
		t.Errorf("unexpected JSON: %s", data)
	}
}

func TestTreeEntry_NullablePointers(t *testing.T) {
	t.Parallel()
	// Directory entry: size/size_human/is_text/etc are all null.
	dir := TreeEntry{
		Name: "subdir",
		Path: "/tmp/subdir",
		Type: "directory",
		// All other pointer fields left nil.
		ChildrenCount: intPtr(5),
	}
	data, _ := json.Marshal(dir)
	mustHaveNull := []string{
		`"size":null`, `"size_human":null`, `"modified_at":null`,
		`"is_text":null`, `"is_compressed":null`, `"compression_format":null`,
		`"is_indexed":null`, `"line_count":null`,
	}
	for _, frag := range mustHaveNull {
		if !strings.Contains(string(data), frag) {
			t.Errorf("expected %s in %s", frag, data)
		}
	}
	if !strings.Contains(string(data), `"children_count":5`) {
		t.Errorf("expected children_count:5 in %s", data)
	}
}

func TestTreeResponse_NullableTotals(t *testing.T) {
	t.Parallel()
	// Stage 9 Round 2 S2: total_size / total_size_human are documented
	// schema fields. When absent they must emit null (was omitempty).
	// Verified against Python's TreeResponse().model_dump() which emits
	// both as null for search-root entries.
	r := TreeResponse{
		Path:         "/tmp",
		Parent:       nil,
		IsSearchRoot: true,
		Entries:      []TreeEntry{},
	}
	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `"total_size":null`) {
		t.Errorf("total_size should emit null when unset, got %s", data)
	}
	if !strings.Contains(string(data), `"total_size_human":null`) {
		t.Errorf("total_size_human should emit null when unset, got %s", data)
	}
	if !strings.Contains(string(data), `"parent":null`) {
		t.Errorf("parent should emit null when nil, got %s", data)
	}
}

func TestDetectorsResponse_EmptyRegistry(t *testing.T) {
	t.Parallel()
	// v1 ships with zero detectors; response envelope must still be a
	// valid JSON with empty lists (not null).
	r := DetectorsResponse{
		Detectors:     []DetectorInfo{},
		Categories:    []CategoryInfo{},
		SeverityScale: []SeverityScaleLevel{},
	}
	data, _ := json.Marshal(r)
	want := `{"detectors":[],"categories":[],"severity_scale":[]}`
	if string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
}

func TestTraceCacheData_JSON(t *testing.T) {
	t.Parallel()
	c := TraceCacheData{
		Version:          2,
		SourcePath:       "/tmp/big.log",
		SourceModifiedAt: "2026-04-17T12:34:56.123456",
		SourceSizeBytes:  1 << 30,
		Patterns:         []string{"error", "warning"},
		PatternsHash:     "a1b2c3d4",
		RgFlags:          []string{"-i"},
		CreatedAt:        "2026-04-17T12:34:57.000000",
		Matches: []TraceCacheMatch{
			{PatternIndex: 0, Offset: 123, LineNumber: 1},
			{PatternIndex: 1, Offset: 456, LineNumber: 4},
		},
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Spot-check key order: version first, matches last.
	s := string(data)
	if !strings.HasPrefix(s, `{"version":`) {
		t.Errorf("expected JSON to start with version, got %s", s[:50])
	}
	// Round-trip.
	var decoded TraceCacheData
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, c) {
		t.Errorf("round-trip mismatch")
	}
}

func TestFileScannedPayload_EventField(t *testing.T) {
	t.Parallel()
	p := FileScannedPayload{
		Event:         HookEventFileScanned,
		RequestID:     "r1",
		FilePath:      "/tmp/a.log",
		FileSizeBytes: 4096,
		ScanTimeMS:    12,
		MatchesCount:  3,
	}
	data, _ := json.Marshal(p)
	if !strings.Contains(string(data), `"event":"file_scanned"`) {
		t.Errorf("expected event:file_scanned, got %s", data)
	}
}

func TestMatchFoundPayload_NullableLineNumber(t *testing.T) {
	t.Parallel()
	withLine := MatchFoundPayload{
		Event:      HookEventMatchFound,
		RequestID:  "r1",
		FilePath:   "/tmp/a.log",
		Pattern:    "error",
		Offset:     123,
		LineNumber: int64Ptr(7),
	}
	data, _ := json.Marshal(withLine)
	if !strings.Contains(string(data), `"line_number":7`) {
		t.Errorf("expected line_number:7, got %s", data)
	}

	noLine := withLine
	noLine.LineNumber = nil
	data, _ = json.Marshal(noLine)
	if !strings.Contains(string(data), `"line_number":null`) {
		t.Errorf("expected line_number:null, got %s", data)
	}
}

func TestIndexResponse_JSON(t *testing.T) {
	t.Parallel()
	r := IndexResponse{
		Success:         true,
		Path:            "/tmp/big.log",
		IndexPath:       strPtr("/home/u/.cache/rx/indexes/big.log_abc.json"),
		LineCount:       int64Ptr(1000000),
		FileSize:        int64Ptr(1 << 30),
		CheckpointCount: intPtr(50),
		TimeSeconds:     float64Ptr(2.5),
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Verify error:null emitted (pointer, not set).
	if !strings.Contains(string(data), `"error":null`) {
		t.Errorf("expected error:null, got %s", data)
	}
}

func TestHealthResponse_MissingPythonPackages(t *testing.T) {
	t.Parallel()
	// Per Appendix A.1, the Go port must NOT emit python_packages.
	h := HealthResponse{
		Status:           "ok",
		RipgrepAvailable: true,
		AppVersion:       "0.1.0",
		GoVersion:        "1.25.0",
		OSInfo:           map[string]string{"system": "Linux"},
		SystemResources:  map[string]any{"cpu_cores": 8},
		GoPackages:       map[string]string{"chi": "v5.1.0"},
		Constants:        map[string]any{},
		Environment:      map[string]string{},
		Hooks:            map[string]any{},
		DocsURL:          "https://example.com",
		SearchRoots:      []string{"/tmp"},
	}
	data, _ := json.Marshal(h)
	if strings.Contains(string(data), "python_packages") {
		t.Errorf("python_packages must not appear: %s", data)
	}
	if strings.Contains(string(data), "python_version") {
		t.Errorf("python_version must not appear: %s", data)
	}
	if !strings.Contains(string(data), `"go_packages"`) {
		t.Errorf("go_packages must appear: %s", data)
	}
	if !strings.Contains(string(data), `"go_version":"1.25.0"`) {
		t.Errorf("go_version must appear: %s", data)
	}
}

func TestCompressRequest_Defaults(t *testing.T) {
	t.Parallel()
	// Verify the JSON shape — important for POST /v1/compress.
	// Stage 9 Round 2 S2: documented schema fields must emit explicit
	// null when unset. Python emits output_path as null and force as
	// false always, so Go must match.
	r := CompressRequest{
		InputPath:        "/tmp/big.log",
		FrameSize:        "4M",
		CompressionLevel: 3,
		BuildIndex:       true,
	}
	data, _ := json.Marshal(r)
	s := string(data)
	for _, frag := range []string{
		`"input_path":"/tmp/big.log"`,
		`"frame_size":"4M"`,
		`"compression_level":3`,
		`"build_index":true`,
		`"output_path":null`,
		`"force":false`,
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("expected %s in %s", frag, s)
		}
	}
}

// Test that ISOTime embedded inside a struct round-trips correctly.
func TestISOTime_AsStructField(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		CreatedAt ISOTime `json:"created_at"`
	}
	data := `{"created_at":"2026-04-17T12:34:56.123456"}`
	var w wrapper
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if w.CreatedAt.Year() != 2026 {
		t.Errorf("year: got %d, want 2026", w.CreatedAt.Year())
	}
	back, _ := json.Marshal(w)
	if string(back) != data {
		t.Errorf("round-trip: got %s, want %s", back, data)
	}
}
