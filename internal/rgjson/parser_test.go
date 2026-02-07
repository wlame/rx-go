package rgjson

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParser_ParseSingleMatch(t *testing.T) {
	jsonOutput := `{"type":"begin","data":{"path":{"text":"test.log"}}}
{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR: something went wrong\n"},"line_number":5,"absolute_offset":123,"submatches":[{"match":{"text":"ERROR"},"start":0,"end":5}]}}
{"type":"end","data":{"path":{"text":"test.log"}}}
`

	parser := NewParser(strings.NewReader(jsonOutput))
	events, err := parser.ParseAll()

	require.NoError(t, err)
	require.Len(t, events, 3)

	// Check begin event
	assert.Equal(t, EventTypeBegin, events[0].Type)

	// Check match event
	assert.Equal(t, EventTypeMatch, events[1].Type)
	assert.NotNil(t, events[1].Data.Lines)
	assert.Equal(t, "ERROR: something went wrong\n", events[1].Data.Lines.Text)
	assert.NotNil(t, events[1].Data.LineNumber)
	assert.Equal(t, int64(5), *events[1].Data.LineNumber)
	assert.Equal(t, int64(123), events[1].Data.AbsoluteOffset)

	// Check end event
	assert.Equal(t, EventTypeEnd, events[2].Type)
}

func TestParser_ParseMultipleMatches(t *testing.T) {
	jsonOutput := `{"type":"begin","data":{"path":{"text":"test.log"}}}
{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR 1\n"},"line_number":1,"absolute_offset":0,"submatches":[]}}
{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR 2\n"},"line_number":2,"absolute_offset":8,"submatches":[]}}
{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR 3\n"},"line_number":3,"absolute_offset":16,"submatches":[]}}
{"type":"end","data":{"path":{"text":"test.log"}}}
`

	parser := NewParser(strings.NewReader(jsonOutput))
	matches, err := parser.ParseMatches()

	require.NoError(t, err)
	assert.Len(t, matches, 3)

	assert.Equal(t, "ERROR 1\n", matches[0].Data.Lines.Text)
	assert.Equal(t, int64(0), matches[0].Data.AbsoluteOffset)

	assert.Equal(t, "ERROR 2\n", matches[1].Data.Lines.Text)
	assert.Equal(t, int64(8), matches[1].Data.AbsoluteOffset)

	assert.Equal(t, "ERROR 3\n", matches[2].Data.Lines.Text)
	assert.Equal(t, int64(16), matches[2].Data.AbsoluteOffset)
}

func TestParser_ParseNoMatches(t *testing.T) {
	jsonOutput := `{"type":"begin","data":{"path":{"text":"test.log"}}}
{"type":"end","data":{"path":{"text":"test.log"}}}
`

	parser := NewParser(strings.NewReader(jsonOutput))
	matches, err := parser.ParseMatches()

	require.NoError(t, err)
	assert.Len(t, matches, 0)
}

func TestParser_ParseWithContext(t *testing.T) {
	jsonOutput := `{"type":"begin","data":{"path":{"text":"test.log"}}}
{"type":"context","data":{"path":{"text":"test.log"},"lines":{"text":"before line\n"},"line_number":4,"absolute_offset":100}}
{"type":"match","data":{"path":{"text":"test.log"},"lines":{"text":"ERROR line\n"},"line_number":5,"absolute_offset":112,"submatches":[]}}
{"type":"context","data":{"path":{"text":"test.log"},"lines":{"text":"after line\n"},"line_number":6,"absolute_offset":123}}
{"type":"end","data":{"path":{"text":"test.log"}}}
`

	parser := NewParser(strings.NewReader(jsonOutput))
	events, err := parser.ParseAll()

	require.NoError(t, err)
	assert.Len(t, events, 5)

	assert.Equal(t, EventTypeContext, events[1].Type)
	assert.Equal(t, EventTypeMatch, events[2].Type)
	assert.Equal(t, EventTypeContext, events[3].Type)
}

func TestParser_Next_EmptyInput(t *testing.T) {
	parser := NewParser(strings.NewReader(""))
	event, err := parser.Next()

	require.Error(t, err)
	assert.Nil(t, event)
}

func TestParser_Next_InvalidJSON(t *testing.T) {
	parser := NewParser(strings.NewReader("not json\n"))
	event, err := parser.Next()

	require.Error(t, err)
	assert.Nil(t, event)
}
