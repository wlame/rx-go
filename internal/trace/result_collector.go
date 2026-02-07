package trace

import (
	"fmt"
	"sort"
	"sync"

	"github.com/wlame/rx-go/pkg/models"
)

// ResultCollector aggregates results from worker pool
type ResultCollector struct {
	patterns      []string
	files         map[string]bool // Track unique files
	patternIDs    map[string]string // Pattern text -> ID (p1, p2, ...)
	fileIDs       map[string]string // File path -> ID (f1, f2, ...)
	matches       []models.Match
	scannedFiles  []string
	skippedFiles  []string
	fileChunks    map[string]int
	errors        []error
	mu            sync.Mutex
}

// NewResultCollector creates a new result collector
func NewResultCollector(patterns []string) *ResultCollector {
	patternIDs := make(map[string]string)
	for i, pattern := range patterns {
		patternIDs[pattern] = fmt.Sprintf("p%d", i+1)
	}

	return &ResultCollector{
		patterns:     patterns,
		files:        make(map[string]bool),
		patternIDs:   patternIDs,
		fileIDs:      make(map[string]string),
		matches:      make([]models.Match, 0),
		scannedFiles: make([]string, 0),
		skippedFiles: make([]string, 0),
		fileChunks:   make(map[string]int),
		errors:       make([]error, 0),
	}
}

// AddResult processes a result from the worker pool
func (c *ResultCollector) AddResult(result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Track file
	if !c.files[result.FilePath] {
		c.files[result.FilePath] = true
		c.scannedFiles = append(c.scannedFiles, result.FilePath)
	}

	// Track chunk count
	currentChunks := c.fileChunks[result.FilePath]
	if result.ChunkID >= currentChunks {
		c.fileChunks[result.FilePath] = result.ChunkID + 1
	}

	// Handle errors
	if result.Error != nil {
		c.errors = append(c.errors, result.Error)
		return
	}

	// Get or create file ID
	fileID, ok := c.fileIDs[result.FilePath]
	if !ok {
		fileID = fmt.Sprintf("f%d", len(c.fileIDs)+1)
		c.fileIDs[result.FilePath] = fileID
	}

	// Add matches
	for _, match := range result.Matches {
		// Determine which pattern matched
		// For now, use p1 (single pattern support)
		// Multi-pattern matching requires pattern detection in MatchResult
		patternID := "p1"

		c.matches = append(c.matches, models.Match{
			Pattern:            patternID,
			File:               fileID,
			Offset:             match.Offset,
			AbsoluteLineNumber: -1, // Will be calculated later if index available
			RelativeLineNumber: match.LineNumber,
			ChunkID:            &result.ChunkID,
		})
	}
}

// AddSkippedFile adds a file to the skipped list
func (c *ResultCollector) AddSkippedFile(filePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skippedFiles = append(c.skippedFiles, filePath)
}

// Finalize sorts matches and prepares final response
func (c *ResultCollector) Finalize() *models.TraceResponse {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sort matches by offset
	sort.Slice(c.matches, func(i, j int) bool {
		if c.matches[i].File != c.matches[j].File {
			return c.matches[i].File < c.matches[j].File
		}
		return c.matches[i].Offset < c.matches[j].Offset
	})

	// Deduplicate matches (in case of overlapping chunks)
	c.matches = c.deduplicateMatches(c.matches)

	// Build pattern ID map (pattern text -> ID)
	patterns := make(map[string]string)
	for pattern, id := range c.patternIDs {
		patterns[id] = pattern
	}

	// Build file ID map (file path -> ID)
	files := make(map[string]string)
	for path, id := range c.fileIDs {
		files[id] = path
	}

	resp := models.NewTraceResponse()
	resp.Patterns = patterns
	resp.Files = files
	resp.Matches = c.matches
	resp.ScannedFiles = c.scannedFiles
	resp.SkippedFiles = c.skippedFiles
	resp.FileChunks = c.fileChunks
	resp.TotalMatches = len(c.matches)

	return resp
}

// deduplicateMatches removes duplicate matches
// Duplicates can occur at chunk boundaries
func (c *ResultCollector) deduplicateMatches(matches []models.Match) []models.Match {
	if len(matches) == 0 {
		return matches
	}

	seen := make(map[string]bool)
	unique := make([]models.Match, 0, len(matches))

	for _, match := range matches {
		// Create key: file + offset
		key := fmt.Sprintf("%s:%d", match.File, match.Offset)

		if !seen[key] {
			seen[key] = true
			unique = append(unique, match)
		}
	}

	return unique
}

// GetErrors returns all collected errors
func (c *ResultCollector) GetErrors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errors
}

// GetMatchCount returns the current number of matches
func (c *ResultCollector) GetMatchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.matches)
}
