package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// ============================================================================
// Engine options
// ============================================================================

// Options bundles the knobs the trace engine exposes. Zero-value is
// valid: each field has a documented default equivalent to Python's
// `parse_paths(..., use_cache=True, use_index=True)` call.
type Options struct {
	MaxResults    *int
	RgExtraArgs   []string
	ContextBefore int
	ContextAfter  int
	NoCache       bool // true = don't read or write the trace cache
	NoIndex       bool // true = don't consult the unified index
	// NoRecursive controls directory expansion: when false (the
	// zero-value default), `rx trace <dir>` walks the full subtree
	// (Python parity starting Stage 9 Round 2 S5). Setting true limits
	// expansion to the top-level entries — CLI `--no-recursive` sets
	// this flag. Passing a single file is unaffected.
	NoRecursive bool
	HookFirer   HookFirer
	// RequestID is carried through to hook payloads. Defaults to
	// empty string when unset; the caller (HTTP or CLI) is responsible
	// for generating one if webhooks are configured.
	RequestID string
}

// applyDefaults fills zero fields with sane defaults.
func (o *Options) applyDefaults() {
	if o.HookFirer == nil {
		o.HookFirer = NoopHookFirer{}
	}
}

// ============================================================================
// Engine (orchestrator)
// ============================================================================

// Engine is the reusable trace engine. Safe for concurrent use: each
// Run call manages its own subprocesses and has no shared state beyond
// the immutable config.
type Engine struct{}

// New returns a zero-value Engine. There's no meaningful construction
// for now; kept as a constructor so future additions (e.g. a metrics
// registry injected for tests) don't break callers.
func New() *Engine { return &Engine{} }

// Run is the top-level search. It:
//
//  1. Resolves each input path (file vs. directory, validated).
//  2. Assigns file IDs ("f1", "f2", ...) and pattern IDs ("p1", ...).
//  3. Classifies each file into one of four buckets:
//     a. Regular        — chunked + parallel ProcessChunk
//     b. Compressed     — ProcessCompressed (single-stream)
//     c. Seekable zstd  — ProcessSeekable (frame-parallel)
//     d. Cache hit      — reconstruct from disk (ReconstructMatchData)
//  4. Runs each bucket through its path, collects MatchRaw.
//  5. Applies IdentifyMatchingPatterns to turn "matched some pattern"
//     into "matched these pattern IDs".
//  6. Sorts, truncates to max_results, resolves absolute line numbers
//     (for chunked files that lack them), and builds a TraceResponse.
//  7. Optionally writes new trace caches for large completed scans.
//  8. Fires OnFile hooks and returns.
//
// Parity with rx-python/src/rx/trace.py::parse_paths is bit-for-bit
// up to intentional differences in logging (structured vs slog) and
// the order in which concurrent batches complete (we sort at the end,
// same as Python).
func (e *Engine) Run(ctx context.Context, req *rxtypes.TraceRequest) (*rxtypes.TraceResponse, error) {
	opts := Options{
		MaxResults:    req.MaxResults,
		RgExtraArgs:   req.RgFlags,
		ContextBefore: ptrIntDeref(req.BeforeContext, 0),
		ContextAfter:  ptrIntDeref(req.AfterContext, 0),
		NoCache:       req.NoCache,
		NoIndex:       req.NoIndex,
	}
	return e.RunWithOptions(ctx, req.Path, req.Patterns, opts)
}

// RunWithOptions is the programmatic entry point used by the CLI and
// tests. It accepts paths + patterns + Options directly (the HTTP
// wrapper calls Run(req)).
func (e *Engine) RunWithOptions(
	ctx context.Context,
	paths []string,
	patterns []string,
	opts Options,
) (*rxtypes.TraceResponse, error) {
	opts.applyDefaults()
	start := time.Now()

	// -------------------------------------------------------------------
	// Phase 0: validate & expand inputs
	// -------------------------------------------------------------------
	filePaths, scannedDirs, skipped := expandPaths(paths, !opts.NoRecursive)
	if len(filePaths) == 0 {
		return &rxtypes.TraceResponse{
			Patterns:     patternIDsMap(patterns),
			Files:        map[string]string{},
			Matches:      []rxtypes.Match{},
			ScannedFiles: []string{},
			// emptyIfNilStrings coerces a nil slice to []string{} so
			// the JSON marshaller emits `[]` instead of `null`. Python
			// emits `[]` for empty lists; see Stage 8 Reviewer 2 High #8.
			SkippedFiles: emptyIfNilStrings(skipped),
			MaxResults:   opts.MaxResults,
			Time:         time.Since(start).Seconds(),
		}, nil
	}

	// -------------------------------------------------------------------
	// Phase 1: assign IDs
	// -------------------------------------------------------------------
	patternIDs := patternIDsMap(patterns)
	patternOrder := make([]string, 0, len(patterns))
	for i := range patterns {
		patternOrder = append(patternOrder, "p"+strconv.Itoa(i+1))
	}
	fileIDs := make(map[string]string, len(filePaths))
	filePathToID := make(map[string]string, len(filePaths))
	for i, fp := range filePaths {
		id := "f" + strconv.Itoa(i+1)
		fileIDs[id] = fp
		filePathToID[fp] = id
	}

	// -------------------------------------------------------------------
	// Phase 2: classify each file
	// -------------------------------------------------------------------
	type fileBucket struct {
		kind        string // "regular" | "compressed" | "seekable" | "cached-regular" | "cached-seekable"
		path        string
		size        int64
		cachedMatch []rxtypes.TraceCacheMatch
		cacheInfo   *CompressedCacheInfo // only for cached-seekable
	}
	var buckets []fileBucket
	fileChunkCounts := make(map[string]int)

	for _, fp := range filePaths {
		fi, err := os.Stat(fp)
		var sz int64
		if err == nil {
			sz = fi.Size()
		}

		// Seekable-zstd first — takes priority over plain zstd.
		if seekable.IsSeekable(fp) {
			if !opts.NoCache {
				if info, cerr := GetCompressedCacheInfo(fp, patterns, opts.RgExtraArgs); cerr == nil {
					buckets = append(buckets, fileBucket{
						kind: "cached-seekable", path: fp, size: sz, cacheInfo: info,
					})
					fileChunkCounts[filePathToID[fp]] = len(info.FramesWithMatches)
					continue
				}
			}
			buckets = append(buckets, fileBucket{kind: "seekable", path: fp, size: sz})
			// Chunk count for seekable = frame count. We fetch it via
			// the seek table; failure falls back to 1.
			if tbl, terr := readSeekTable(fp); terr == nil {
				fileChunkCounts[filePathToID[fp]] = tbl.NumFrames
			} else {
				fileChunkCounts[filePathToID[fp]] = 1
			}
			continue
		}
		if compression.IsCompressed(fp) {
			buckets = append(buckets, fileBucket{kind: "compressed", path: fp, size: sz})
			fileChunkCounts[filePathToID[fp]] = 1
			continue
		}
		// Regular files — try cache if large enough.
		if !opts.NoCache && sz >= largeFileThresholdBytes() {
			if cm, cerr := GetCachedMatches(fp, patterns, opts.RgExtraArgs); cerr == nil {
				buckets = append(buckets, fileBucket{
					kind: "cached-regular", path: fp, size: sz, cachedMatch: cm,
				})
				fileChunkCounts[filePathToID[fp]] = 0 // 0 = served from cache
				continue
			}
		}
		buckets = append(buckets, fileBucket{kind: "regular", path: fp, size: sz})
	}

	// -------------------------------------------------------------------
	// Phase 3: execute each bucket
	// -------------------------------------------------------------------
	var allMatches []rxtypes.Match
	var allContexts []contextWithFile
	// Track matches-to-be-cached per file (only set for regular / seekable
	// buckets that are candidates for cache write).
	cacheCandidates := map[string][]rxtypes.Match{}
	compressedCacheCandidates := map[string][]rxtypes.Match{}
	frameIndexByOffset := map[string]map[int64]int{} // per-file map

	for _, b := range buckets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// Stop early once max_results is hit.
		if opts.MaxResults != nil && len(allMatches) >= *opts.MaxResults {
			break
		}
		fileID := filePathToID[b.path]
		fileStart := time.Now()

		switch b.kind {
		case "regular":
			tasks, terr := CreateFileTasks(b.path)
			if terr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			fileChunkCounts[fileID] = len(tasks)
			// Single-chunk flag: when a file lives in exactly ONE chunk,
			// the ripgrep output's relative line number IS the absolute
			// line number (no chunk offset to add). Stage 9 Round 2 R1-B2
			// fix: Python's single-chunk path computes this correctly;
			// Go must match so the CLI JSON output carries real line
			// numbers instead of -1 sentinels.
			singleChunk := len(tasks) == 1
			// Pass the REMAINING cap (opts.MaxResults minus already-collected
			// matches) so ProcessAllChunks can cooperatively cancel as
			// soon as this file alone contributes enough to close the
			// overall budget. remainingResults returns nil when no cap
			// was set at all. Stage 9 Round 5 R5-B2 fix.
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			allChunkMatches, allChunkContexts, perr := ProcessAllChunks(
				ctx, tasks, patternIDs, patternOrder,
				opts.RgExtraArgs, opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if perr != nil && !errors.Is(perr, context.Canceled) {
				skipped = append(skipped, b.path)
				continue
			}
			// Flatten + identify.
			for _, chunkMatches := range allChunkMatches {
				for _, rm := range chunkMatches {
					matchedIDs := IdentifyMatchingPatterns(
						rm.LineText, rm.Submatches,
						patternIDs, patternOrder, opts.RgExtraArgs,
					)
					for _, pid := range matchedIDs {
						m := toMatch(pid, fileID, rm)
						if singleChunk && m.RelativeLineNumber != nil {
							// Single-chunk: absolute == relative. Python parity.
							m.AbsoluteLineNumber = *m.RelativeLineNumber
						}
						allMatches = append(allMatches, m)
						cacheCandidates[b.path] = append(cacheCandidates[b.path], m)
						opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
							Pattern: patternIDs[pid], Offset: m.Offset,
							LineNumber: int64(*m.RelativeLineNumber),
						})
					}
				}
			}
			for _, chunkContexts := range allChunkContexts {
				for _, rc := range chunkContexts {
					absLine := -1
					if singleChunk {
						// Context lines share the same rule: in single-chunk
						// files the chunker's "line number" already IS the
						// file's absolute line number.
						absLine = rc.LineNumber
					}
					allContexts = append(allContexts, contextWithFile{
						fileID: fileID,
						ctx: rxtypes.ContextLine{
							RelativeLineNumber: rc.LineNumber,
							AbsoluteLineNumber: absLine,
							LineText:           rc.LineText,
							AbsoluteOffset:     rc.Offset,
						},
					})
				}
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size, countMatchesForFile(allMatches, fileID))
		case "compressed":
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			format, _ := compression.DetectFromPath(b.path)
			if format == compression.FormatNone {
				skipped = append(skipped, b.path)
				continue
			}
			rawMatches, rawContexts, _, cerr := ProcessCompressed(
				ctx, b.path, format,
				patternIDs, patternOrder, opts.RgExtraArgs,
				opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if cerr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			for _, rm := range rawMatches {
				matchedIDs := IdentifyMatchingPatterns(
					rm.LineText, rm.Submatches,
					patternIDs, patternOrder, opts.RgExtraArgs,
				)
				for _, pid := range matchedIDs {
					m := toMatch(pid, fileID, rm)
					allMatches = append(allMatches, m)
					opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
						Pattern: patternIDs[pid], Offset: m.Offset,
						LineNumber: int64(*m.RelativeLineNumber),
					})
				}
			}
			for _, rc := range rawContexts {
				allContexts = append(allContexts, contextWithFile{
					fileID: fileID,
					ctx: rxtypes.ContextLine{
						RelativeLineNumber: rc.LineNumber,
						AbsoluteLineNumber: -1,
						LineText:           rc.LineText,
						AbsoluteOffset:     rc.Offset,
					},
				})
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size,
				countMatchesForFile(allMatches, fileID))
		case "seekable":
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			rawMatches, rawContexts, _, serr := ProcessSeekable(
				ctx, b.path,
				patternIDs, patternOrder, opts.RgExtraArgs,
				opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if serr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			// Track frame_index per offset so we can cache later.
			fm := frameIndexByOffset[b.path]
			if fm == nil {
				fm = map[int64]int{}
				frameIndexByOffset[b.path] = fm
			}
			for _, rm := range rawMatches {
				matchedIDs := IdentifyMatchingPatterns(
					rm.LineText, rm.Submatches,
					patternIDs, patternOrder, opts.RgExtraArgs,
				)
				for _, pid := range matchedIDs {
					m := toMatch(pid, fileID, rm)
					allMatches = append(allMatches, m)
					compressedCacheCandidates[b.path] = append(compressedCacheCandidates[b.path], m)
					opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
						Pattern: patternIDs[pid], Offset: m.Offset,
						LineNumber: int64(*m.RelativeLineNumber),
					})
				}
			}
			for _, rc := range rawContexts {
				allContexts = append(allContexts, contextWithFile{
					fileID: fileID,
					ctx: rxtypes.ContextLine{
						RelativeLineNumber: rc.LineNumber,
						AbsoluteLineNumber: -1,
						LineText:           rc.LineText,
						AbsoluteOffset:     rc.Offset,
					},
				})
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size,
				countMatchesForFile(allMatches, fileID))
		case "cached-regular":
			for _, cm := range b.cachedMatch {
				match, ctxLines, rerr := ReconstructMatchData(
					b.path, cm, patterns, patternIDs,
					fileID, opts.RgExtraArgs,
					opts.ContextBefore, opts.ContextAfter, !opts.NoIndex,
				)
				if rerr != nil {
					continue
				}
				allMatches = append(allMatches, match)
				for _, cl := range ctxLines {
					allContexts = append(allContexts, contextWithFile{fileID: fileID, ctx: cl})
				}
				opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
					Pattern: patterns[cm.PatternIndex], Offset: cm.Offset, LineNumber: cm.LineNumber,
				})
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size, len(b.cachedMatch))
		case "cached-seekable":
			// The seekable fast-path cache uses the same reconstruction
			// machinery; decompressing only frames_with_matches.
			if b.cacheInfo == nil {
				continue
			}
			for _, cm := range b.cacheInfo.Matches {
				match, ctxLines, rerr := ReconstructMatchData(
					b.path, cm, patterns, patternIDs,
					fileID, opts.RgExtraArgs,
					opts.ContextBefore, opts.ContextAfter, !opts.NoIndex,
				)
				if rerr != nil {
					continue
				}
				// Tag as compressed for symmetry; callers that look at
				// is_compressed (frontend) need this flag.
				allMatches = append(allMatches, match)
				for _, cl := range ctxLines {
					allContexts = append(allContexts, contextWithFile{fileID: fileID, ctx: cl})
				}
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size, len(b.cacheInfo.Matches))
		}
	}

	// -------------------------------------------------------------------
	// Phase 4: sort, truncate, finalize
	// -------------------------------------------------------------------
	sort.SliceStable(allMatches, func(i, j int) bool {
		if allMatches[i].File != allMatches[j].File {
			return allMatches[i].File < allMatches[j].File
		}
		if allMatches[i].Offset != allMatches[j].Offset {
			return allMatches[i].Offset < allMatches[j].Offset
		}
		return allMatches[i].Pattern < allMatches[j].Pattern
	})
	if opts.MaxResults != nil && len(allMatches) > *opts.MaxResults {
		allMatches = allMatches[:*opts.MaxResults]
	}

	contextDict := buildContextDict(allMatches, allContexts, opts.ContextBefore, opts.ContextAfter)

	// -------------------------------------------------------------------
	// Phase 5: write caches for large completed regular scans
	// -------------------------------------------------------------------
	maxResultsHit := opts.MaxResults != nil && len(allMatches) >= *opts.MaxResults
	if !opts.NoCache && !maxResultsHit {
		for path, matches := range cacheCandidates {
			var size int64
			if fi, err := os.Stat(path); err == nil {
				size = fi.Size()
			}
			if ShouldCache(size, opts.MaxResults, true, false) {
				data, berr := BuildCache(path, patterns, opts.RgExtraArgs, matches, nil, "")
				if berr == nil {
					_ = SaveCache(CachePath(path, patterns, opts.RgExtraArgs), data)
				}
			}
		}
		for path, matches := range compressedCacheCandidates {
			var size int64
			if fi, err := os.Stat(path); err == nil {
				size = fi.Size()
			}
			if ShouldCache(size, opts.MaxResults, true, true) {
				data, berr := BuildCache(path, patterns, opts.RgExtraArgs, matches,
					frameIndexByOffset[path], "zstd-seekable")
				if berr == nil {
					_ = SaveCache(CachePath(path, patterns, opts.RgExtraArgs), data)
				}
			}
		}
	}

	// -------------------------------------------------------------------
	// Phase 6: response assembly
	// -------------------------------------------------------------------
	elapsed := time.Since(start)
	// Stage 9 Round 2 S6: gated helper — no-op in CLI mode.
	prometheus.AddMatchesFound(len(allMatches))

	// All nullable slice fields must serialize as [] (not null) when
	// empty — the JSON contract says they're arrays. nil slices
	// marshal as null in Go, breaking frontend iteration expectations.
	// Centralize via emptyIfNilStrings for consistency. See Stage 8
	// Reviewer 2 High #8.
	resp := &rxtypes.TraceResponse{
		Patterns:     patternIDs,
		Files:        fileIDs,
		Matches:      emptyIfNilMatches(allMatches),
		ScannedFiles: emptyIfNilStrings(scannedFilesOutput(filePaths, scannedDirs)),
		SkippedFiles: emptyIfNilStrings(dedupStrings(skipped)),
		MaxResults:   opts.MaxResults,
		Time:         elapsed.Seconds(),
	}
	if len(fileChunkCounts) > 0 {
		resp.FileChunks = fileChunkCounts
	}
	if len(contextDict) > 0 {
		resp.ContextLines = contextDict
	}
	if opts.ContextBefore > 0 {
		b := opts.ContextBefore
		resp.BeforeContext = &b
	}
	if opts.ContextAfter > 0 {
		a := opts.ContextAfter
		resp.AfterContext = &a
	}
	return resp, nil
}

// ============================================================================
// Helpers
// ============================================================================

// contextWithFile pairs a ContextLine with the fileID it belongs to —
// Python threads these tuples through the same way.
type contextWithFile struct {
	fileID string
	ctx    rxtypes.ContextLine
}

// expandPaths splits input paths into (files, dirs-scanned, skipped).
//
// Stage 9 Round 2 S5 + R1-B7 fix: recursive defaults to TRUE (Python
// parity). When recursive is false (CLI `--no-recursive`), only the
// top-level directory entries are scanned.
//
// Binary files and unreadable files go into `skipped`. Compressed
// archives (gzip/xz/bz2/zst) are treated as text because ripgrep can
// read them via decompressors.
func expandPaths(paths []string, recursive bool) (files, scannedDirs, skipped []string) {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, p)
			continue
		}
		if fi.IsDir() {
			scannedDirs = append(scannedDirs, p)
			if recursive {
				// Walk the subtree. Directory-stat errors terminate the
				// walk for THAT subtree but don't abort the whole scan.
				walkErr := walkDirForTextFiles(p, &files, &skipped)
				if walkErr != nil {
					// Falling through to the non-recursive path would mask
					// the error. Append to skipped so the caller sees it.
					skipped = append(skipped, p)
				}
				continue
			}
			// Non-recursive: top-level entries only.
			entries, rerr := os.ReadDir(p)
			if rerr != nil {
				skipped = append(skipped, p)
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				full := p
				if !strings.HasSuffix(full, "/") {
					full += "/"
				}
				full += entry.Name()
				if !isTextFile(full) {
					skipped = append(skipped, full)
					continue
				}
				files = append(files, full)
			}
			continue
		}
		if !isTextFile(p) {
			skipped = append(skipped, p)
			continue
		}
		files = append(files, p)
	}
	return files, scannedDirs, skipped
}

// walkDirForTextFiles walks dir recursively, appending text files to
// *files and non-text / unreadable files to *skipped. We don't use
// filepath.Walk because its func-callback style makes it awkward to
// thread two output slices; a simple explicit recursion is clearer.
func walkDirForTextFiles(dir string, files, skipped *[]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		full := dir
		if !strings.HasSuffix(full, "/") {
			full += "/"
		}
		full += entry.Name()
		if entry.IsDir() {
			// Subdirectory: recurse. Don't propagate the error — a
			// permission-denied subdir should skip, not fail the outer
			// scan.
			_ = walkDirForTextFiles(full, files, skipped)
			continue
		}
		if !isTextFile(full) {
			*skipped = append(*skipped, full)
			continue
		}
		*files = append(*files, full)
	}
	return nil
}

// isTextFile returns true when the first 8 KB of the file contains no
// null bytes. Mirrors Python's is_text_file in file_utils.py — we
// special-case compressed files as "text" since ripgrep (via decompressor)
// will read them.
func isTextFile(path string) bool {
	if compression.IsCompressed(path) {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sample := make([]byte, 8192)
	n, _ := f.Read(sample)
	for i := 0; i < n; i++ {
		if sample[i] == 0 {
			return false
		}
	}
	return true
}

// patternIDsMap builds the "p1" -> "pattern" map. Single place so
// engine + worker agree on id format.
func patternIDsMap(patterns []string) map[string]string {
	out := make(map[string]string, len(patterns))
	for i, p := range patterns {
		out["p"+strconv.Itoa(i+1)] = p
	}
	return out
}

// toMatch turns a MatchRaw + pattern_id + file_id into the final
// rxtypes.Match. Wraps pointer-conversion for line number + line text.
func toMatch(pid, fileID string, rm MatchRaw) rxtypes.Match {
	line := rm.LineNumber
	text := rm.LineText
	return rxtypes.Match{
		Pattern:            pid,
		File:               fileID,
		Offset:             rm.Offset,
		RelativeLineNumber: &line,
		AbsoluteLineNumber: -1, // resolved later if possible
		LineText:           &text,
		Submatches:         rm.Submatches,
	}
}

// remainingResults returns nil if the caller didn't set a cap, else
// the positive number of remaining slots. When slots are exhausted
// it returns a pointer to zero — callers treat that as "skip".
func remainingResults(max *int, have int) *int {
	if max == nil {
		return nil
	}
	r := *max - have
	if r < 0 {
		r = 0
	}
	return &r
}

// countMatchesForFile returns how many matches currently belong to
// fileID. Used for OnFile hook payloads.
func countMatchesForFile(matches []rxtypes.Match, fileID string) int {
	n := 0
	for _, m := range matches {
		if m.File == fileID {
			n++
		}
	}
	return n
}

// fireOnFile invokes the OnFile hook for a finished file scan. Separate
// helper so the engine's switch statement stays readable.
func fireOnFile(ctx context.Context, hf HookFirer, path string, fileStart time.Time, size int64, matches int) {
	if hf == nil {
		return
	}
	elapsedMS := int(time.Since(fileStart) / time.Millisecond)
	hf.OnFile(ctx, path, FileInfo{
		FileSizeBytes: size,
		ScanTimeMS:    elapsedMS,
		MatchesCount:  matches,
	})
}

// buildContextDict groups context lines around each match, keyed by
// "<pattern>:<file>:<offset>". Mirrors Python's group-and-expand
// logic in parse_multiple_files_multipattern.
func buildContextDict(
	matches []rxtypes.Match,
	contexts []contextWithFile,
	contextBefore, contextAfter int,
) map[string][]rxtypes.ContextLine {
	out := make(map[string][]rxtypes.ContextLine)
	for _, m := range matches {
		if m.RelativeLineNumber == nil {
			continue
		}
		matchLine := *m.RelativeLineNumber
		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)

		// Build the window: the matched line itself + every context
		// line within [-contextBefore, +contextAfter] of matchLine for
		// the SAME file.
		matchedText := ""
		if m.LineText != nil {
			matchedText = *m.LineText
		}
		window := []rxtypes.ContextLine{{
			RelativeLineNumber: matchLine,
			AbsoluteLineNumber: m.AbsoluteLineNumber,
			LineText:           matchedText,
			AbsoluteOffset:     m.Offset,
		}}
		if contextBefore > 0 || contextAfter > 0 {
			width := contextBefore
			if contextAfter > width {
				width = contextAfter
			}
			for _, cwf := range contexts {
				if cwf.fileID != m.File {
					continue
				}
				d := cwf.ctx.RelativeLineNumber - matchLine
				if d == 0 {
					continue // matched line already added
				}
				if d < -width || d > width {
					continue
				}
				window = append(window, cwf.ctx)
			}
		}
		sort.SliceStable(window, func(i, j int) bool {
			return window[i].RelativeLineNumber < window[j].RelativeLineNumber
		})
		out[key] = window
	}
	return out
}

// scannedFilesOutput returns the "scanned_files" field value for the
// response. Python sets it to the full file list ONLY when at least
// one input path was a directory; otherwise it's an empty slice.
func scannedFilesOutput(filePaths, scannedDirs []string) []string {
	if len(scannedDirs) == 0 {
		return []string{}
	}
	return append([]string(nil), filePaths...)
}

// emptyIfNilStrings coerces a nil []string to []string{} so JSON
// serialization emits [] instead of null. Python's json.dumps always
// emits [] for empty lists; Go's encoding/json emits null for nil
// slices. Use this on every nullable slice that reaches the TraceResponse.
//
// Defining a dedicated helper rather than an inline `if nil { = [] }`
// pattern at every call site keeps the parity invariant in one place
// and makes it obvious where the frontend contract is enforced.
func emptyIfNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// emptyIfNilMatches is the []rxtypes.Match specialization of
// emptyIfNilStrings. Same rationale: nil → [] for JSON parity.
//
// We write a separate helper per element type because Go's generics
// would require `emptyIfNil[T any](s []T) []T` and that would bump
// the function's call-site signature just enough to make the intent
// less obvious at the grep level. Two small helpers with clear names
// beat one generic helper that everyone has to look up.
func emptyIfNilMatches(s []rxtypes.Match) []rxtypes.Match {
	if s == nil {
		return []rxtypes.Match{}
	}
	return s
}

// dedupStrings preserves order and removes duplicates. Used on the
// skipped-files list because some buckets may re-add the same file
// when it fails multiple classification checks.
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// ptrIntDeref returns *p if non-nil, else def. Used for the optional
// int fields of TraceRequest.
func ptrIntDeref(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// ParsePaths is a convenience wrapper matching the Python entry point
// name. New code should call Engine{}.Run directly; this is provided
// so CLI glue code can keep the Python call-site vocabulary.
func ParsePaths(
	ctx context.Context,
	paths []string,
	patterns []string,
	opts Options,
) (*rxtypes.TraceResponse, error) {
	return (&Engine{}).RunWithOptions(ctx, paths, patterns, opts)
}

// For future-stretch use: a `//go:build debug` variant could inject
// debug-file writes here. We intentionally leave that hook in as a
// single symbol so it's easy to reintroduce Python's RX_DEBUG path.
var _ = config.DebugMode
