package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewTraceResponse(t *testing.T) {
	resp := NewTraceResponse()

	// Verify all maps are initialized (not nil)
	assert.NotNil(t, resp.Patterns, "Patterns map should be initialized")
	assert.NotNil(t, resp.Files, "Files map should be initialized")
	assert.NotNil(t, resp.FileChunks, "FileChunks map should be initialized")
	assert.NotNil(t, resp.ContextLines, "ContextLines map should be initialized")

	// Verify all slices are initialized (not nil)
	assert.NotNil(t, resp.Matches, "Matches slice should be initialized")
	assert.NotNil(t, resp.ScannedFiles, "ScannedFiles slice should be initialized")
	assert.NotNil(t, resp.SkippedFiles, "SkippedFiles slice should be initialized")

	// Verify all collections are empty
	assert.Empty(t, resp.Patterns, "Patterns map should be empty")
	assert.Empty(t, resp.Files, "Files map should be empty")
	assert.Empty(t, resp.Matches, "Matches slice should be empty")
	assert.Empty(t, resp.ScannedFiles, "ScannedFiles slice should be empty")
	assert.Empty(t, resp.SkippedFiles, "SkippedFiles slice should be empty")
	assert.Empty(t, resp.FileChunks, "FileChunks map should be empty")
	assert.Empty(t, resp.ContextLines, "ContextLines map should be empty")

	// Verify default values for other fields
	assert.Equal(t, "", resp.RequestID, "RequestID should be empty string")
	assert.Nil(t, resp.Paths, "Paths should be nil (not initialized)")
	assert.Equal(t, 0.0, resp.Time, "Time should be 0.0")
	assert.Equal(t, 0, resp.BeforeContext, "BeforeContext should be 0")
	assert.Equal(t, 0, resp.AfterContext, "AfterContext should be 0")
	assert.Equal(t, 0, resp.TotalMatches, "TotalMatches should be 0")
	assert.Equal(t, 0.0, resp.SearchTimeMs, "SearchTimeMs should be 0.0")
	assert.False(t, resp.CacheHit, "CacheHit should be false")
}
