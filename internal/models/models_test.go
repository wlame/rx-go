package models

import (
	"testing"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ID helpers -------------------------------------------------------------

func TestPatternID(t *testing.T) {
	assert.Equal(t, "p1", PatternID(1))
	assert.Equal(t, "p2", PatternID(2))
	assert.Equal(t, "p10", PatternID(10))
}

func TestFileID(t *testing.T) {
	assert.Equal(t, "f1", FileID(1))
	assert.Equal(t, "f2", FileID(2))
	assert.Equal(t, "f10", FileID(10))
}

// --- TraceResponse JSON round-trip ------------------------------------------

func TestTraceResponse_json_roundtrip(t *testing.T) {
	resp := NewTraceResponse("req-123", []string{"/var/log"})
	resp.Time = 0.456
	resp.Patterns["p1"] = "error"
	resp.Files["f1"] = "/var/log/app.log"
	lineText := "error: something failed"
	resp.Matches = append(resp.Matches, Match{
		Pattern:            "p1",
		File:               "f1",
		Offset:             100,
		AbsoluteLineNumber: 42,
		LineText:           &lineText,
	})

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded TraceResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "req-123", decoded.RequestID)
	assert.Equal(t, []string{"/var/log"}, decoded.Path)
	assert.Equal(t, 0.456, decoded.Time)
	assert.Equal(t, "error", decoded.Patterns["p1"])
	assert.Equal(t, "/var/log/app.log", decoded.Files["f1"])
	require.Len(t, decoded.Matches, 1)
	assert.Equal(t, "p1", decoded.Matches[0].Pattern)
	assert.Equal(t, 100, decoded.Matches[0].Offset)
	assert.Equal(t, 42, decoded.Matches[0].AbsoluteLineNumber)
	require.NotNil(t, decoded.Matches[0].LineText)
	assert.Equal(t, "error: something failed", *decoded.Matches[0].LineText)
}

func TestTraceResponse_empty_slices_not_null(t *testing.T) {
	resp := NewTraceResponse("req-1", []string{"/tmp"})

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	// matches, scanned_files, skipped_files must be [] not null.
	assert.Contains(t, raw, `"matches":[]`)
	assert.Contains(t, raw, `"scanned_files":[]`)
	assert.Contains(t, raw, `"skipped_files":[]`)
}

func TestTraceResponse_null_fields_serialize_as_null(t *testing.T) {
	resp := NewTraceResponse("req-2", []string{"/tmp"})

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	// Nullable pointer fields should serialize as null.
	assert.Contains(t, raw, `"max_results":null`)
	assert.Contains(t, raw, `"file_chunks":null`)
	assert.Contains(t, raw, `"context_lines":null`)
	assert.Contains(t, raw, `"before_context":null`)
	assert.Contains(t, raw, `"after_context":null`)
	assert.Contains(t, raw, `"cli_command":null`)
}

// --- Match null fields ------------------------------------------------------

func TestMatch_null_fields(t *testing.T) {
	m := NewMatch("p1", "f1", 200)

	data, err := json.Marshal(m)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"relative_line_number":null`)
	assert.Contains(t, raw, `"line_text":null`)
	assert.Contains(t, raw, `"submatches":null`)
	assert.Contains(t, raw, `"absolute_line_number":-1`)
}

// --- SamplesResponse --------------------------------------------------------

func TestSamplesResponse_json_roundtrip(t *testing.T) {
	resp := NewSamplesResponse("/var/log/app.log", 3, 3)
	resp.Offsets["100"] = 5
	resp.Samples["100"] = []string{"line before", "matched line", "line after"}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded SamplesResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/var/log/app.log", decoded.Path)
	assert.Equal(t, 3, decoded.BeforeCtx)
	assert.Equal(t, 3, decoded.AfterCtx)
	assert.Equal(t, 5, decoded.Offsets["100"])
	assert.Equal(t, []string{"line before", "matched line", "line after"}, decoded.Samples["100"])
	assert.False(t, decoded.IsCompressed)
	assert.Nil(t, decoded.CompressionFormat)
}

func TestSamplesResponse_empty_maps_not_null(t *testing.T) {
	resp := NewSamplesResponse("/tmp/f.log", 0, 0)

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"offsets":{}`)
	assert.Contains(t, raw, `"lines":{}`)
	assert.Contains(t, raw, `"samples":{}`)
}

// --- ComplexityResponse stub ------------------------------------------------

func TestComplexityResponse_stub(t *testing.T) {
	resp := NewStubComplexityResponse("(a+)+")

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded ComplexityResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "(a+)+", decoded.Regex)
	assert.Equal(t, float64(0), decoded.Score)
	assert.Equal(t, "not_implemented", decoded.RiskLevel)
	assert.Equal(t, "not_implemented", decoded.ComplexityClass)
	assert.Equal(t, "not_implemented", decoded.ComplexityNotation)
	assert.Equal(t, "unknown", decoded.Level)
	assert.Equal(t, "not_implemented", decoded.Risk)
	assert.Equal(t, 5, decoded.PatternLength)
	assert.Equal(t, [2]bool{false, false}, decoded.HasAnchors)
}

func TestComplexityResponse_empty_slices_not_null(t *testing.T) {
	resp := NewStubComplexityResponse("test")

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"issues":[]`)
	assert.Contains(t, raw, `"recommendations":[]`)
	assert.Contains(t, raw, `"warnings":[]`)
}

// --- DetectorsResponse ------------------------------------------------------

func TestDetectorsResponse_empty_lists(t *testing.T) {
	resp := NewDetectorsResponse()

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"detectors":[]`)
	assert.Contains(t, raw, `"categories":[]`)
	assert.Contains(t, raw, `"severity_scale":[]`)
}

func TestDetectorsResponse_json_roundtrip(t *testing.T) {
	resp := NewDetectorsResponse()
	resp.Detectors = append(resp.Detectors, DetectorInfo{
		Name:        "traceback",
		Category:    "error",
		Description: "Detects stack traces",
		SeverityRange: SeverityRange{Min: 0.5, Max: 1.0},
		Examples:    []string{"Traceback (most recent call last):"},
	})
	resp.Categories = append(resp.Categories, CategoryInfo{
		Name:        "error",
		Description: "Error patterns",
		Detectors:   []string{"traceback"},
	})
	resp.SeverityScale = append(resp.SeverityScale, SeverityScaleLevel{
		Min:         0.8,
		Max:         1.0,
		Label:       "critical",
		Description: "Critical issues",
	})

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded DetectorsResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	require.Len(t, decoded.Detectors, 1)
	assert.Equal(t, "traceback", decoded.Detectors[0].Name)
	assert.Equal(t, 0.5, decoded.Detectors[0].SeverityRange.Min)
	require.Len(t, decoded.Categories, 1)
	assert.Equal(t, "error", decoded.Categories[0].Name)
	require.Len(t, decoded.SeverityScale, 1)
	assert.Equal(t, "critical", decoded.SeverityScale[0].Label)
}

// --- TaskResponse / TaskStatusResponse --------------------------------------

func TestTaskResponse_json_roundtrip(t *testing.T) {
	started := "2026-04-04T12:00:00Z"
	resp := TaskResponse{
		TaskID:    "task-1",
		Status:    "running",
		Message:   "Indexing file",
		Path:      "/var/log/app.log",
		StartedAt: &started,
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded TaskResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "task-1", decoded.TaskID)
	assert.Equal(t, "running", decoded.Status)
	require.NotNil(t, decoded.StartedAt)
	assert.Equal(t, started, *decoded.StartedAt)
}

func TestTaskStatusResponse_null_fields(t *testing.T) {
	resp := TaskStatusResponse{
		TaskID:    "task-2",
		Status:    "queued",
		Path:      "/tmp/f.log",
		Operation: "index",
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"started_at":null`)
	assert.Contains(t, raw, `"completed_at":null`)
	assert.Contains(t, raw, `"error":null`)
	assert.Contains(t, raw, `"result":null`)
}

// --- RequestInfo ------------------------------------------------------------

func TestRequestInfo_constructor(t *testing.T) {
	ri := NewRequestInfo("req-99", []string{"/var/log"}, []string{"error"})
	assert.Equal(t, "req-99", ri.RequestID)
	assert.Equal(t, []string{"/var/log"}, ri.Paths)
	assert.Equal(t, []string{"error"}, ri.Patterns)
	assert.Equal(t, 0, ri.TotalMatches)
	assert.Nil(t, ri.MaxResults)
}

// --- FileIndex --------------------------------------------------------------

func TestFileIndex_json_roundtrip(t *testing.T) {
	idx := NewFileIndex(2, IndexTypeRegular, "/var/log/app.log", "2026-01-01T00:00:00Z", 1024)

	data, err := json.Marshal(idx)
	require.NoError(t, err)

	var decoded FileIndex
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, 2, decoded.Version)
	assert.Equal(t, IndexTypeRegular, decoded.IndexType)
	assert.Equal(t, "/var/log/app.log", decoded.SourcePath)
	assert.NotNil(t, decoded.LineIndex)
	assert.Empty(t, decoded.LineIndex)
}

func TestFileIndex_empty_line_index_not_null(t *testing.T) {
	idx := NewFileIndex(2, IndexTypeRegular, "/tmp/f.log", "2026-01-01T00:00:00Z", 0)

	data, err := json.Marshal(idx)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"line_index":[]`)
}

func TestFileIndex_nullable_fields(t *testing.T) {
	idx := NewFileIndex(2, IndexTypeCompressed, "/tmp/f.gz", "2026-01-01T00:00:00Z", 500)

	data, err := json.Marshal(idx)
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"build_time_seconds":null`)
	assert.Contains(t, raw, `"analysis":null`)
	assert.Contains(t, raw, `"frames":null`)
	assert.Contains(t, raw, `"compression_format":null`)
}

// --- IndexType / FileType string values -------------------------------------

func TestIndexType_string_values(t *testing.T) {
	assert.Equal(t, "regular", string(IndexTypeRegular))
	assert.Equal(t, "compressed", string(IndexTypeCompressed))
	assert.Equal(t, "seekable_zstd", string(IndexTypeSeekableZstd))
}

func TestFileType_string_values(t *testing.T) {
	assert.Equal(t, "text", string(FileTypeText))
	assert.Equal(t, "binary", string(FileTypeBinary))
	assert.Equal(t, "compressed", string(FileTypeCompressed))
	assert.Equal(t, "seekable_zstd", string(FileTypeSeekableZstd))
}
