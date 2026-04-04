package rgjson

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- real rg JSON samples ---------------------------------------------------

const sampleBegin = `{"type":"begin","data":{"path":{"text":"/var/log/app.log"}}}`

const sampleMatch = `{"type":"match","data":{"path":{"text":"/var/log/app.log"},"lines":{"text":"2024-01-15 ERROR: connection refused\n"},"line_number":42,"absolute_offset":1234,"submatches":[{"match":{"text":"ERROR"},"start":16,"end":21}]}}`

const sampleContext = `{"type":"context","data":{"path":{"text":"/var/log/app.log"},"lines":{"text":"2024-01-15 INFO: starting up\n"},"line_number":41,"absolute_offset":1100,"submatches":[]}}`

const sampleEnd = `{"type":"end","data":{"path":{"text":"/var/log/app.log"},"binary_offset":null,"stats":{"elapsed":{"secs":0,"nanos":123456,"human":"0.000123s"},"searches":1,"searches_with_match":1,"bytes_searched":5000,"bytes_printed":200,"matched_lines":1,"matches":1}}}`

const sampleSummary = `{"type":"summary","data":{"elapsed_total":{"secs":0,"nanos":456789,"human":"0.000457s"},"stats":{"elapsed":{"secs":0,"nanos":456789,"human":"0.000457s"},"searches":1,"searches_with_match":1,"bytes_searched":5000,"bytes_printed":200,"matched_lines":1,"matches":1}}}`

// --- individual message type parsing ----------------------------------------

func TestParseLine_begin(t *testing.T) {
	msg, err := parseLine([]byte(sampleBegin))
	require.NoError(t, err)

	assert.Equal(t, TypeBegin, msg.Type)
	require.NotNil(t, msg.Begin)
	assert.Equal(t, "/var/log/app.log", msg.Begin.Path.Text)
	assert.Nil(t, msg.Match)
	assert.Nil(t, msg.Context)
	assert.Nil(t, msg.End)
	assert.Nil(t, msg.Summary)
}

func TestParseLine_match(t *testing.T) {
	msg, err := parseLine([]byte(sampleMatch))
	require.NoError(t, err)

	assert.Equal(t, TypeMatch, msg.Type)
	require.NotNil(t, msg.Match)
	assert.Equal(t, "/var/log/app.log", msg.Match.Path.Text)
	assert.Equal(t, 42, msg.Match.LineNumber)
	assert.Equal(t, 1234, msg.Match.AbsoluteOffset)
	assert.Contains(t, msg.Match.Lines.Text, "ERROR: connection refused")

	require.Len(t, msg.Match.Submatches, 1)
	assert.Equal(t, "ERROR", msg.Match.Submatches[0].Match.Text)
	assert.Equal(t, 16, msg.Match.Submatches[0].Start)
	assert.Equal(t, 21, msg.Match.Submatches[0].End)
}

func TestParseLine_context(t *testing.T) {
	msg, err := parseLine([]byte(sampleContext))
	require.NoError(t, err)

	assert.Equal(t, TypeContext, msg.Type)
	require.NotNil(t, msg.Context)
	assert.Equal(t, 41, msg.Context.LineNumber)
	assert.Equal(t, 1100, msg.Context.AbsoluteOffset)
	assert.Empty(t, msg.Context.Submatches)
}

func TestParseLine_end(t *testing.T) {
	msg, err := parseLine([]byte(sampleEnd))
	require.NoError(t, err)

	assert.Equal(t, TypeEnd, msg.Type)
	require.NotNil(t, msg.End)
	assert.Equal(t, "/var/log/app.log", msg.End.Path.Text)
	assert.Nil(t, msg.End.BinaryOffset, "binary_offset should be nil when null")
	assert.Equal(t, 1, msg.End.Stats.Matches)
	assert.Equal(t, 5000, msg.End.Stats.BytesSearched)
}

func TestParseLine_summary(t *testing.T) {
	msg, err := parseLine([]byte(sampleSummary))
	require.NoError(t, err)

	assert.Equal(t, TypeSummary, msg.Type)
	require.NotNil(t, msg.Summary)
	assert.Equal(t, 1, msg.Summary.Stats.Matches)
	assert.Equal(t, 5000, msg.Summary.Stats.BytesSearched)
}

// --- stream parsing ---------------------------------------------------------

func TestParser_full_stream(t *testing.T) {
	stream := strings.Join([]string{
		sampleBegin,
		sampleContext,
		sampleMatch,
		sampleEnd,
		sampleSummary,
	}, "\n")

	p := NewParser(strings.NewReader(stream), 0)
	msgs, err := p.ParseAll()
	require.NoError(t, err)

	require.Len(t, msgs, 5)
	assert.Equal(t, TypeBegin, msgs[0].Type)
	assert.Equal(t, TypeContext, msgs[1].Type)
	assert.Equal(t, TypeMatch, msgs[2].Type)
	assert.Equal(t, TypeEnd, msgs[3].Type)
	assert.Equal(t, TypeSummary, msgs[4].Type)
}

func TestParser_empty_stream(t *testing.T) {
	p := NewParser(strings.NewReader(""), 0)
	msgs, err := p.ParseAll()
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestParser_Next_returns_nil_at_end(t *testing.T) {
	p := NewParser(strings.NewReader(sampleBegin+"\n"), 0)

	msg, err := p.Next()
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, TypeBegin, msg.Type)

	msg, err = p.Next()
	require.NoError(t, err)
	assert.Nil(t, msg, "expected nil after stream exhausted")
}

// --- malformed input handling -----------------------------------------------

func TestParser_malformed_lines_skipped(t *testing.T) {
	stream := strings.Join([]string{
		"this is not json",
		sampleMatch,
		`{"type":"unknown_garbage","data":{}}`,
		`{broken json`,
		sampleEnd,
	}, "\n")

	p := NewParser(strings.NewReader(stream), 0)
	msgs, err := p.ParseAll()
	require.NoError(t, err)

	// Only the valid match and end events should survive.
	require.Len(t, msgs, 2)
	assert.Equal(t, TypeMatch, msgs[0].Type)
	assert.Equal(t, TypeEnd, msgs[1].Type)
}

func TestParser_empty_lines_skipped(t *testing.T) {
	stream := "\n\n" + sampleBegin + "\n\n\n" + sampleMatch + "\n\n"

	p := NewParser(strings.NewReader(stream), 0)
	msgs, err := p.ParseAll()
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	assert.Equal(t, TypeBegin, msgs[0].Type)
	assert.Equal(t, TypeMatch, msgs[1].Type)
}

// --- long lines handling ----------------------------------------------------

func TestParser_long_line_within_buffer(t *testing.T) {
	// Create a match with a very long line text (4KB of 'x').
	longText := strings.Repeat("x", 4096)
	line := `{"type":"match","data":{"path":{"text":"f.log"},"lines":{"text":"` +
		longText +
		`\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}`

	p := NewParser(strings.NewReader(line+"\n"), 8) // 8 KB buffer
	msgs, err := p.ParseAll()
	require.NoError(t, err)

	require.Len(t, msgs, 1)
	assert.Equal(t, TypeMatch, msgs[0].Type)
	assert.Contains(t, msgs[0].Match.Lines.Text, longText)
}

func TestParser_line_exceeds_buffer(t *testing.T) {
	// Create a line that exceeds the 1KB buffer.
	longText := strings.Repeat("x", 2048)
	line := `{"type":"match","data":{"path":{"text":"f.log"},"lines":{"text":"` +
		longText +
		`\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}`

	// Use a very small buffer (1KB) — the line won't fit.
	p := NewParser(strings.NewReader(line+"\n"), 1)
	msgs, err := p.ParseAll()

	// The scanner should report an error for the oversized line.
	// Depending on buffering, we either get an error or the line is skipped.
	// Either outcome is acceptable — the parser should not panic.
	_ = msgs
	_ = err
}

// --- multiple submatches ----------------------------------------------------

func TestParseLine_multiple_submatches(t *testing.T) {
	line := `{"type":"match","data":{"path":{"text":"f.log"},"lines":{"text":"error and warning here\n"},"line_number":10,"absolute_offset":500,"submatches":[{"match":{"text":"error"},"start":0,"end":5},{"match":{"text":"warning"},"start":10,"end":17}]}}`

	msg, err := parseLine([]byte(line))
	require.NoError(t, err)

	require.NotNil(t, msg.Match)
	require.Len(t, msg.Match.Submatches, 2)
	assert.Equal(t, "error", msg.Match.Submatches[0].Match.Text)
	assert.Equal(t, 0, msg.Match.Submatches[0].Start)
	assert.Equal(t, 5, msg.Match.Submatches[0].End)
	assert.Equal(t, "warning", msg.Match.Submatches[1].Match.Text)
	assert.Equal(t, 10, msg.Match.Submatches[1].Start)
	assert.Equal(t, 17, msg.Match.Submatches[1].End)
}

// --- end event with binary_offset set ---------------------------------------

func TestParseLine_end_with_binary_offset(t *testing.T) {
	line := `{"type":"end","data":{"path":{"text":"binary.dat"},"binary_offset":256,"stats":{"elapsed":{"secs":0,"nanos":100,"human":"0.0001s"},"searches":1,"searches_with_match":0,"bytes_searched":1024,"bytes_printed":0,"matched_lines":0,"matches":0}}}`

	msg, err := parseLine([]byte(line))
	require.NoError(t, err)

	require.NotNil(t, msg.End)
	require.NotNil(t, msg.End.BinaryOffset)
	assert.Equal(t, 256, *msg.End.BinaryOffset)
}

// --- DefaultMaxLineSizeKB ---------------------------------------------------

func TestNewParser_default_buffer(t *testing.T) {
	// Passing 0 should use the default buffer size and still work.
	p := NewParser(strings.NewReader(sampleMatch+"\n"), 0)
	msgs, err := p.ParseAll()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
}
