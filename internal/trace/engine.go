package trace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/security"
	"github.com/wlame/rx-go/pkg/models"
)

// Engine orchestrates the search process
type Engine struct {
	cfg            *config.Config
	security       *security.SearchRootsManager
	indexStore     *index.Store
	contextExtractor *ContextExtractor
}

// NewEngine creates a new search engine
func NewEngine(cfg *config.Config) *Engine {
	return &Engine{
		cfg:            cfg,
		security:       security.NewSearchRootsManager(cfg.SearchRoots),
		indexStore:     index.NewStore(cfg.GetIndexCacheDir()),
		contextExtractor: NewContextExtractor(),
	}
}

// Search executes a search request
func (e *Engine) Search(ctx context.Context, req *models.TraceRequest) (*models.TraceResponse, error) {
	startTime := time.Now()

	// Validate request
	if err := e.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Expand paths
	files, err := e.expandPaths(req.Paths, req.SkipBinary)
	if err != nil {
		return nil, fmt.Errorf("failed to expand paths: %w", err)
	}

	if len(files) == 0 {
		return e.emptyResponse(req, startTime), nil
	}

	// Create result collector
	collector := NewResultCollector(req.Patterns)

	// Create worker pool
	maxResults := req.MaxResults
	if maxResults == 0 {
		maxResults = int(^uint(0) >> 1) // Max int
	}

	pool := NewWorkerPool(e.cfg.MaxWorkers, maxResults, req.Patterns, req.CaseSensitive)
	pool.Start()

	// Create and submit tasks
	chunker := NewChunker(e.cfg)
	taskCount := 0

	for _, filePath := range files {
		tasks, err := chunker.CreateTasks(filePath)
		if err != nil {
			collector.AddSkippedFile(filePath)
			continue
		}

		for _, task := range tasks {
			if !pool.SubmitTask(task) {
				break // Context cancelled or pool closed
			}
			taskCount++
		}
	}

	// Close task channel
	pool.Close()

	// Collect results
	for result := range pool.Results() {
		collector.AddResult(result)

		// Check if we've exceeded max results
		if maxResults > 0 && collector.GetMatchCount() >= maxResults {
			pool.Cancel()
		}
	}

	// Finalize response
	resp := collector.Finalize()
	resp.Paths = req.Paths
	resp.BeforeContext = req.BeforeContext
	resp.AfterContext = req.AfterContext
	resp.Time = time.Since(startTime).Seconds()
	resp.SearchTimeMs = time.Since(startTime).Seconds() * 1000

	// Extract context lines if requested
	if (req.BeforeContext > 0 || req.AfterContext > 0) && len(resp.Matches) > 0 {
		if err := e.addContextLines(resp, req.BeforeContext, req.AfterContext); err != nil {
			// Non-fatal: log error but continue
			// TODO: Add proper logging
		}
	}

	// Add absolute line numbers using index if available
	if !e.cfg.NoIndex {
		e.addAbsoluteLineNumbers(resp)
	}

	return resp, nil
}

// validateRequest validates the search request
func (e *Engine) validateRequest(req *models.TraceRequest) error {
	if len(req.Paths) == 0 {
		return fmt.Errorf("at least one path is required")
	}

	if len(req.Patterns) == 0 {
		return fmt.Errorf("at least one pattern is required")
	}

	// Validate paths are within search roots
	for _, path := range req.Paths {
		absPath, err := security.NormalizePath(path)
		if err != nil {
			return fmt.Errorf("invalid path %s: %w", path, err)
		}

		if !e.security.IsAllowed(absPath) {
			return fmt.Errorf("path %s is not within allowed search roots", path)
		}
	}

	return nil
}

// expandPaths expands paths to a list of files
func (e *Engine) expandPaths(paths []string, skipBinary bool) ([]string, error) {
	var files []string
	seen := make(map[string]bool)

	for _, path := range paths {
		absPath, err := security.NormalizePath(path)
		if err != nil {
			return nil, err
		}

		fileInfo, err := os.Stat(absPath)
		if err != nil {
			return nil, fmt.Errorf("failed to stat %s: %w", path, err)
		}

		if fileInfo.IsDir() {
			// Expand directory
			dirFiles, err := e.expandDirectory(absPath, skipBinary)
			if err != nil {
				return nil, err
			}
			for _, f := range dirFiles {
				if !seen[f] {
					seen[f] = true
					files = append(files, f)
				}
			}
		} else {
			// Single file
			if skipBinary && e.isBinaryFile(absPath) {
				continue
			}
			if !seen[absPath] {
				seen[absPath] = true
				files = append(files, absPath)
			}
		}
	}

	return files, nil
}

// expandDirectory recursively expands a directory
func (e *Engine) expandDirectory(dirPath string, skipBinary bool) ([]string, error) {
	var files []string
	count := 0

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible files
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Check max files limit
		count++
		if count > e.cfg.MaxFiles {
			return fmt.Errorf("directory contains more than %d files (limit: RX_MAX_FILES)", e.cfg.MaxFiles)
		}

		// Skip binary files
		if skipBinary && e.isBinaryFile(path) {
			return nil
		}

		files = append(files, path)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return files, nil
}

// isBinaryFile checks if a file is binary
func (e *Engine) isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true // Assume binary if can't open
	}
	defer f.Close()

	// Read first 512 bytes
	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil {
		return true
	}

	// Check for null bytes (common in binary files)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true
		}
	}

	return false
}

// emptyResponse creates an empty response
func (e *Engine) emptyResponse(req *models.TraceRequest, startTime time.Time) *models.TraceResponse {
	resp := models.NewTraceResponse()
	resp.Paths = req.Paths
	resp.BeforeContext = req.BeforeContext
	resp.AfterContext = req.AfterContext
	resp.Time = time.Since(startTime).Seconds()
	resp.SearchTimeMs = time.Since(startTime).Seconds() * 1000
	return resp
}

// addContextLines extracts and adds context lines to the response
func (e *Engine) addContextLines(resp *models.TraceResponse, beforeContext, afterContext int) error {
	// Group matches by file
	fileMatches := make(map[string][]models.Match)
	for _, match := range resp.Matches {
		fileID := match.File
		fileMatches[fileID] = append(fileMatches[fileID], match)
	}

	// Extract context for each file
	allContextLines := make(map[string][]models.ContextLine)

	for fileID, matches := range fileMatches {
		filePath := resp.Files[fileID]

		// Try to load index for this file
		var fileIndex *models.FileIndex
		if !e.cfg.NoIndex {
			idx, err := e.indexStore.Load(filePath)
			if err == nil {
				fileIndex = idx
			}
		}

		// Extract context
		contextLines, err := e.contextExtractor.ExtractContext(
			filePath,
			matches,
			beforeContext,
			afterContext,
			fileIndex,
		)
		if err != nil {
			continue // Skip on error
		}

		// Merge into response
		for key, lines := range contextLines {
			allContextLines[key] = lines
		}
	}

	resp.ContextLines = allContextLines
	return nil
}

// addAbsoluteLineNumbers adds absolute line numbers to matches using indexes
func (e *Engine) addAbsoluteLineNumbers(resp *models.TraceResponse) {
	// Group matches by file
	fileMatches := make(map[string][]*models.Match)
	for i := range resp.Matches {
		fileID := resp.Matches[i].File
		fileMatches[fileID] = append(fileMatches[fileID], &resp.Matches[i])
	}

	// Process each file
	for fileID, matches := range fileMatches {
		filePath := resp.Files[fileID]

		// Try to load index
		idx, err := e.indexStore.Load(filePath)
		if err != nil {
			continue // Skip if no index
		}

		// Update matches with absolute line numbers
		for _, match := range matches {
			lineNum := index.FindLineNumber(idx.LineIndex, match.Offset)
			if lineNum > 0 {
				match.AbsoluteLineNumber = lineNum
			}
		}
	}
}
