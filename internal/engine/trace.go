// trace.go is the top-level orchestrator for the trace search engine.
//
// It coordinates file scanning, chunk planning, parallel worker dispatch, result merging,
// pattern identification, and line number resolution. This is the Go equivalent of
// Python's parse_paths() in trace.py.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wlame/rx/internal/compression"
	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/models"
)

// TraceRequest holds all parameters for a trace search operation.
type TraceRequest struct {
	Paths         []string          // File or directory paths to search.
	Patterns      []string          // Regex patterns to search for.
	MaxResults    int               // Maximum results (0 = unlimited).
	RgExtraArgs   []string          // Extra arguments passed through to rg.
	ContextBefore int               // Lines of context before each match.
	ContextAfter  int               // Lines of context after each match.
	UseCache      bool              // Whether to use trace cache (Phase 4).
	UseIndex      bool              // Whether to use file indexes for line resolution.
	GetIndex      GetIndex          // Optional index lookup function (Phase 4).
}

// Trace runs a parallel search across files and returns a TraceResponse.
//
// Algorithm (mirrors Python's parse_paths):
//  1. Assign pattern IDs (p1, p2, ...).
//  2. Scan and classify files via fileutil.ScanDirectory.
//  3. Validate all patterns via rg.
//  4. For each file: plan chunks.
//  5. Fast path: single file with single chunk — direct SearchChunk, no goroutines.
//  6. Parallel path: errgroup with bounded concurrency, one task per chunk.
//  7. Collect results, check limit, cancel on limit reached.
//  8. Heap-merge results from all chunks.
//  9. Truncate to MaxResults.
//  10. Phase 2 pattern identification.
//  11. Assign file IDs, resolve line numbers.
//  12. Build and return TraceResponse.
func Trace(ctx context.Context, req TraceRequest) (*models.TraceResponse, error) {
	cfg := config.Load()
	startTime := time.Now()

	// Step 1: Assign pattern IDs.
	patternIDs := make(map[string]string, len(req.Patterns))
	for i, p := range req.Patterns {
		patternIDs[models.PatternID(i+1)] = p
	}

	// Step 2: Scan and classify files.
	var allFiles []fileutil.FileInfo
	var skippedFiles []string
	var scannedDirs []string

	for _, path := range req.Paths {
		stat, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("path not found: %s", path)
		}

		if stat.IsDir() {
			scannedDirs = append(scannedDirs, path)
			files, skipped, err := fileutil.ScanDirectory(path, cfg)
			if err != nil {
				return nil, fmt.Errorf("scan directory %s: %w", path, err)
			}
			allFiles = append(allFiles, files...)
			skippedFiles = append(skippedFiles, skipped...)
		} else {
			// Single file — create FileInfo directly.
			allFiles = append(allFiles, fileutil.FileInfo{
				Path:           path,
				Size:           stat.Size(),
				Classification: fileutil.ClassText,
			})
		}
	}

	// Build file ID mapping. File IDs are assigned in order: f1, f2, ...
	fileIDs := make(map[string]string, len(allFiles))   // "f1" -> "/path/to/file"
	fileIDReverse := make(map[string]string, len(allFiles)) // "/path/to/file" -> "f1"
	for i, fi := range allFiles {
		fid := models.FileID(i + 1)
		fileIDs[fid] = fi.Path
		fileIDReverse[fi.Path] = fid
	}

	// Handle empty file list.
	if len(allFiles) == 0 {
		resp := models.NewTraceResponse("", req.Paths)
		resp.Patterns = patternIDs
		resp.SkippedFiles = skippedFiles
		resp.Time = time.Since(startTime).Seconds()
		return &resp, nil
	}

	// Step 3: Validate all patterns via rg.
	for _, pattern := range req.Patterns {
		if err := ValidatePattern(pattern); err != nil {
			return nil, err
		}
	}

	// Step 4: Plan chunks for each file.
	type fileChunks struct {
		path   string
		file   *os.File
		chunks []Chunk
	}

	var planned []fileChunks
	fileChunkCounts := make(map[string]int) // file ID -> chunk count

	// Collect compressed files separately — they use a different search strategy.
	type compressedFile struct {
		path string
		info fileutil.FileInfo
	}
	var compressedFiles []compressedFile

	for _, fi := range allFiles {
		// Route compressed files to the compressed search path.
		if fi.Classification == fileutil.ClassCompressed {
			compressedFiles = append(compressedFiles, compressedFile{path: fi.Path, info: fi})
			continue
		}

		if fi.Classification != fileutil.ClassText {
			continue
		}

		chunks, err := PlanChunks(fi.Path, fi.Size, &cfg)
		if err != nil {
			slog.Warn("chunk planning failed, skipping file",
				"path", fi.Path, "error", err)
			continue
		}

		fid := fileIDReverse[fi.Path]
		fileChunkCounts[fid] = len(chunks)

		// Open the file once — all chunk workers share the same file descriptor
		// via SectionReader (which uses ReadAt, safe for concurrent use).
		f, err := os.Open(fi.Path)
		if err != nil {
			slog.Warn("cannot open file, skipping",
				"path", fi.Path, "error", err)
			continue
		}
		planned = append(planned, fileChunks{path: fi.Path, file: f, chunks: chunks})
	}

	// Ensure all file handles are closed when we're done.
	defer func() {
		for _, fc := range planned {
			fc.file.Close()
		}
	}()

	// Count total chunks for deciding fast path vs parallel path.
	totalChunks := 0
	for _, fc := range planned {
		totalChunks += len(fc.chunks)
	}

	// Step 5-7: Execute search — fast path or parallel path.
	var allResults [][]models.Match

	if totalChunks <= 1 {
		// Fast path: single chunk, no goroutines, no channels, no heap merge.
		for _, fc := range planned {
			for _, chunk := range fc.chunks {
				matches, err := SearchChunk(ctx, fc.file, chunk, req.Patterns, req.RgExtraArgs, cfg.MaxLineSizeKB)
				if err != nil && ctx.Err() == nil {
					slog.Warn("chunk search failed",
						"path", fc.path, "chunk", chunk.Index, "error", err)
					continue
				}
				// Tag matches with file ID.
				fid := fileIDReverse[fc.path]
				for i := range matches {
					matches[i].File = fid
				}
				allResults = append(allResults, matches)
			}
		}
	} else {
		// Parallel path: use errgroup with bounded concurrency.
		searchCtx, searchCancel := context.WithCancel(ctx)
		defer searchCancel()

		var mu sync.Mutex
		matchCount := 0

		g, gctx := errgroup.WithContext(searchCtx)
		g.SetLimit(cfg.MaxSubprocesses)

		for _, fc := range planned {
			for _, chunk := range fc.chunks {
				// Capture loop variables for the goroutine closure.
				fc := fc
				chunk := chunk

				g.Go(func() error {
					// Check if we've already hit the limit before starting work.
					if req.MaxResults > 0 {
						mu.Lock()
						if matchCount >= req.MaxResults {
							mu.Unlock()
							return nil
						}
						mu.Unlock()
					}

					matches, err := SearchChunk(gctx, fc.file, chunk, req.Patterns, req.RgExtraArgs, cfg.MaxLineSizeKB)
					if err != nil {
						if gctx.Err() != nil {
							return nil // context cancelled — expected during eager termination
						}
						slog.Warn("chunk search failed",
							"path", fc.path, "chunk", chunk.Index, "error", err)
						return nil // don't fail the whole group for one chunk
					}

					// Tag matches with file ID.
					fid := fileIDReverse[fc.path]
					for i := range matches {
						matches[i].File = fid
					}

					mu.Lock()
					allResults = append(allResults, matches)
					matchCount += len(matches)

					// Eager termination: if we've collected enough matches, cancel
					// remaining workers. We'll truncate to the exact limit after merge.
					if req.MaxResults > 0 && matchCount >= req.MaxResults {
						searchCancel()
					}
					mu.Unlock()

					return nil
				})
			}
		}

		// Wait for all workers to finish (or be cancelled).
		_ = g.Wait()
	}

	// Step 7b: Search compressed files.
	for _, cf := range compressedFiles {
		fid := fileIDReverse[cf.path]

		// Detect full compression format (including seekable zstd distinction).
		format, detectErr := compression.Detect(cf.path)
		if detectErr != nil {
			slog.Warn("compression detection failed, skipping",
				"path", cf.path, "error", detectErr)
			continue
		}

		var matches []models.Match
		var searchErr error

		if format == compression.FormatSeekableZstd {
			matches, searchErr = SearchSeekableZstd(ctx, cf.path, req.Patterns, req.RgExtraArgs, &cfg)
		} else {
			matches, searchErr = SearchCompressedFile(ctx, cf.path, req.Patterns, req.RgExtraArgs, cfg.MaxLineSizeKB)
		}

		if searchErr != nil {
			if ctx.Err() != nil {
				break // Context cancelled — stop processing.
			}
			slog.Warn("compressed file search failed",
				"path", cf.path, "error", searchErr)
			continue
		}

		// Tag matches with file ID.
		for i := range matches {
			matches[i].File = fid
		}

		allResults = append(allResults, matches)
	}

	// Step 8: Heap-merge results from all chunks.
	merged := MergeResults(allResults)

	// Step 9: Truncate to MaxResults.
	if req.MaxResults > 0 {
		merged = TruncateResults(merged, req.MaxResults)
	}

	// Step 10: Phase 2 pattern identification.
	merged = IdentifyPatterns(merged, patternIDs, req.RgExtraArgs)

	// Re-sort after pattern identification (which may have expanded matches).
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].File != merged[j].File {
			return merged[i].File < merged[j].File
		}
		if merged[i].Offset != merged[j].Offset {
			return merged[i].Offset < merged[j].Offset
		}
		return merged[i].Pattern < merged[j].Pattern
	})

	// Re-truncate after pattern expansion.
	if req.MaxResults > 0 {
		merged = TruncateResults(merged, req.MaxResults)
	}

	// Step 11: Resolve line numbers.
	merged = ResolveLineNumbers(merged, fileIDs, req.GetIndex)

	// Step 12: Build TraceResponse.
	resp := models.NewTraceResponse("", req.Paths)
	resp.Patterns = patternIDs
	resp.Files = fileIDs
	resp.Matches = merged
	resp.Time = time.Since(startTime).Seconds()

	if len(scannedDirs) > 0 {
		var scanned []string
		for _, fi := range allFiles {
			scanned = append(scanned, fi.Path)
		}
		resp.ScannedFiles = scanned
	}
	resp.SkippedFiles = skippedFiles

	if req.MaxResults > 0 {
		resp.MaxResults = &req.MaxResults
	}

	fc := fileChunkCounts
	resp.FileChunks = &fc

	return &resp, nil
}
