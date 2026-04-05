package trace

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/wlame/rx-go/pkg/models"
)

// ResultCollectorFixed is an improved version with better concurrency
type ResultCollectorFixed struct {
	patterns      []string
	files         map[string]bool
	patternIDs    map[string]string
	fileIDs       map[string]string
	matches       []models.Match
	scannedFiles  []string
	skippedFiles  []string
	fileChunks    map[string]int
	errors        []error
	mu            sync.Mutex
	matchCount    int64
	maxResults    int
	cancelFunc    context.CancelFunc

	// Batch processing
	batchSize     int
	matchBuffer   []MatchResult
	bufferMu      sync.Mutex
}

// NewResultCollectorFixed creates an improved result collector
func NewResultCollectorFixed(patterns []string, maxResults int, cancelFunc context.CancelFunc) *ResultCollectorFixed {
	patternIDs := make(map[string]string)
	for i, pattern := range patterns {
		patternIDs[pattern] = fmt.Sprintf("p%d", i+1)
	}

	return &ResultCollectorFixed{
		patterns:     patterns,
		files:        make(map[string]bool),
		patternIDs:   patternIDs,
		fileIDs:      make(map[string]string),
		matches:      make([]models.Match, 0, 10000), // Pre-allocate
		scannedFiles: make([]string, 0, 100),
		skippedFiles: make([]string, 0),
		fileChunks:   make(map[string]int),
		errors:       make([]error, 0),
		matchCount:   0,
		maxResults:   maxResults,
		cancelFunc:   cancelFunc,
		batchSize:    100, // Process 100 matches at a time
		matchBuffer:  make([]MatchResult, 0, 100),
	}
}

// ConsumeMatches consumes matches with batching for better performance
func (c *ResultCollectorFixed) ConsumeMatches(matchChan <-chan MatchResult) {
	for match := range matchChan {
		// Fast path: check limit with atomic read (no lock)
		if c.maxResults > 0 {
			currentCount := atomic.LoadInt64(&c.matchCount)
			if int(currentCount) >= c.maxResults {
				if c.cancelFunc != nil {
					c.cancelFunc()
				}
				// Drain channel to prevent worker deadlock
				continue
			}
		}

		// Add to buffer (minimal lock time)
		c.bufferMu.Lock()
		c.matchBuffer = append(c.matchBuffer, match)
		bufferLen := len(c.matchBuffer)
		c.bufferMu.Unlock()

		// Process batch when full
		if bufferLen >= c.batchSize {
			c.processBatch()
		}
	}

	// Process remaining matches in buffer
	c.processBatch()
}

// processBatch processes accumulated matches in one go
func (c *ResultCollectorFixed) processBatch() {
	// Get buffer (minimal lock time)
	c.bufferMu.Lock()
	if len(c.matchBuffer) == 0 {
		c.bufferMu.Unlock()
		return
	}
	batch := c.matchBuffer
	c.matchBuffer = make([]MatchResult, 0, c.batchSize)
	c.bufferMu.Unlock()

	// Process batch (main lock)
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, match := range batch {
		// Track file
		if !c.files[match.FilePath] {
			c.files[match.FilePath] = true
			c.scannedFiles = append(c.scannedFiles, match.FilePath)
		}

		// Get or create file ID
		fileID, ok := c.fileIDs[match.FilePath]
		if !ok {
			fileID = fmt.Sprintf("f%d", len(c.fileIDs)+1)
			c.fileIDs[match.FilePath] = fileID
		}

		// Pattern ID (multi-pattern needs enhancement)
		patternID := "p1"

		// Add match
		c.matches = append(c.matches, models.Match{
			Pattern:            patternID,
			File:               fileID,
			Offset:             match.Offset,
			AbsoluteLineNumber: -1,
			RelativeLineNumber: match.LineNumber,
			ChunkID:            nil,
		})

		// Increment atomic counter
		atomic.AddInt64(&c.matchCount, 1)

		// Check limit after batch
		if c.maxResults > 0 && len(c.matches) >= c.maxResults {
			if c.cancelFunc != nil {
				c.cancelFunc()
			}
			break
		}
	}
}

// AddMatch adds a single match (for compressed files)
func (c *ResultCollectorFixed) AddMatch(match MatchResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Track file
	if !c.files[match.FilePath] {
		c.files[match.FilePath] = true
		c.scannedFiles = append(c.scannedFiles, match.FilePath)
	}

	// Get or create file ID
	fileID, ok := c.fileIDs[match.FilePath]
	if !ok {
		fileID = fmt.Sprintf("f%d", len(c.fileIDs)+1)
		c.fileIDs[match.FilePath] = fileID
	}

	patternID := "p1"

	c.matches = append(c.matches, models.Match{
		Pattern:            patternID,
		File:               fileID,
		Offset:             match.Offset,
		AbsoluteLineNumber: -1,
		RelativeLineNumber: match.LineNumber,
		ChunkID:            nil,
	})

	atomic.AddInt64(&c.matchCount, 1)
}

// AddSkippedFile adds a file to the skipped list
func (c *ResultCollectorFixed) AddSkippedFile(filePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skippedFiles = append(c.skippedFiles, filePath)
}

// Finalize sorts matches and prepares final response
func (c *ResultCollectorFixed) Finalize() *models.TraceResponse {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sort matches by offset
	sort.Slice(c.matches, func(i, j int) bool {
		if c.matches[i].File != c.matches[j].File {
			return c.matches[i].File < c.matches[j].File
		}
		return c.matches[i].Offset < c.matches[j].Offset
	})

	// Deduplicate matches
	c.matches = c.deduplicateMatches(c.matches)

	// Build maps
	patterns := make(map[string]string)
	for pattern, id := range c.patternIDs {
		patterns[id] = pattern
	}

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

// deduplicateMatches removes duplicates
func (c *ResultCollectorFixed) deduplicateMatches(matches []models.Match) []models.Match {
	if len(matches) == 0 {
		return matches
	}

	seen := make(map[string]bool)
	unique := make([]models.Match, 0, len(matches))

	for _, match := range matches {
		key := fmt.Sprintf("%s:%d", match.File, match.Offset)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, match)
		}
	}

	return unique
}

// GetErrors returns all collected errors
func (c *ResultCollectorFixed) GetErrors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errors
}

// GetMatchCount returns the current number of matches
func (c *ResultCollectorFixed) GetMatchCount() int {
	return int(atomic.LoadInt64(&c.matchCount))
}
