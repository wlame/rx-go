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
	"github.com/wlame/rx-go/internal/index"
	sandbox "github.com/wlame/rx-go/internal/paths" // aliased: local vars named `paths`
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
	// (Python parity). Setting true limits
	// expansion to the top-level entries — CLI `--no-recursive` sets
	// this flag. Passing a single file is unaffected.
	NoRecursive bool
	HookFirer   HookFirer
	// RequestID is carried through to hook payloads. Defaults to
	// empty string when unset; the caller (HTTP or CLI) is responsible
	// for generating one if webhooks are configured.
	RequestID string

	// afterScan, when set, runs once the chunks of a plain file have
	// been scanned and before the file's trace cache is written. It is a
	// test seam: a test uses it to change the file at exactly that
	// moment, the way a live log grows while rx reads it. Being
	// unexported, it can only be set from inside this package, and
	// production code leaves it nil. A file answered from the trace
	// cache is not scanned, so the function is not called for it.
	afterScan func(path string)
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
		// Nothing to search still answers with the request it was
		// given: an empty request id and a null path made a skipped
		// file indistinguishable from a malformed response.
		return &rxtypes.TraceResponse{
			RequestID:    opts.RequestID,
			Path:         emptyIfNilStrings(append([]string(nil), paths...)),
			Patterns:     patternIDsMap(patterns),
			Files:        map[string]string{},
			Matches:      []rxtypes.Match{},
			ScannedFiles: []string{},
			// emptyIfNilStrings coerces a nil slice to []string{} so
			// the JSON marshaller emits `[]` instead of `null`. Python
			// emits `[]` for empty lists.
			SkippedFiles: emptyIfNilStrings(skipped),
			MaxResults:   opts.MaxResults,
			Time:         time.Since(start).Seconds(),
			// Empty objects rather than null, as on the main path.
			FileChunks:   map[string]int{},
			ContextLines: map[string][]rxtypes.ContextLine{},
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
		kind string // "regular" | "compressed" | "seekable" | "cached-regular" | "cached-seekable"
		path string
		size int64
		// info is the stat taken when the file was classified, nil when
		// the stat failed. A plain file's chunks are planned from it and
		// its trace cache is stamped with it, so the cache describes the
		// bytes the scan covered and nothing the file gained later. For
		// every kind, a nil info keeps the file out of the per-file
		// metrics, which have no size to report for it.
		info        os.FileInfo
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
		} else {
			fi = nil
		}

		// Seekable-zstd first — takes priority over plain zstd.
		if seekable.IsSeekable(fp) {
			if !opts.NoCache {
				if info, cerr := GetCompressedCacheInfo(fp, patterns, opts.RgExtraArgs); cerr == nil {
					buckets = append(buckets, fileBucket{
						kind: "cached-seekable", path: fp, size: sz, info: fi, cacheInfo: info,
					})
					fileChunkCounts[filePathToID[fp]] = info.ChunkCount
					continue
				}
			}
			buckets = append(buckets, fileBucket{kind: "seekable", path: fp, size: sz, info: fi})
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
			buckets = append(buckets, fileBucket{kind: "compressed", path: fp, size: sz, info: fi})
			fileChunkCounts[filePathToID[fp]] = 1
			continue
		}
		// Regular files — try cache if large enough.
		if !opts.NoCache && sz >= largeFileThresholdBytes() {
			if cached, cerr := GetCachedScan(fp, patterns, opts.RgExtraArgs); cerr == nil {
				buckets = append(buckets, fileBucket{
					kind: "cached-regular", path: fp, size: sz, info: fi, cachedMatch: cached.Matches,
				})
				// The chunk count of the scan that wrote the cache, so a
				// cache hit answers exactly what that scan answered.
				fileChunkCounts[filePathToID[fp]] = cached.ChunkCount
				continue
			}
		}
		buckets = append(buckets, fileBucket{kind: "regular", path: fp, size: sz, info: fi})
	}

	// -------------------------------------------------------------------
	// Phase 3: execute each bucket
	// -------------------------------------------------------------------
	var allMatches []rxtypes.Match
	var allContexts []contextWithFile
	// Completed scans whose answer goes into the trace cache, keyed by
	// path. A file gets an entry only when caching was wanted before its
	// scan started (see scanToCache) and the scan read every chunk.
	toCache := map[string]*ScannedFile{}

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
		if b.info != nil {
			// gated helper — no-op in CLI mode.
			prometheus.RecordFileScanned(b.size)
		}

		switch b.kind {
		case "regular":
			if b.info == nil {
				skipped = append(skipped, b.path)
				continue
			}
			// Plan from the stat taken at classification and record that
			// stat as the file's identity, before any byte is read.
			tasks, terr := planFileTasks(b.path, b.info.Size())
			if terr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			cacheEntry := scanToCache(opts, b.path, b.info, "")
			if cacheEntry != nil {
				cacheEntry.Chunks = len(tasks)
			}
			fileChunkCounts[fileID] = len(tasks)
			prometheus.RecordParallelTasks(len(tasks))
			// Pass the REMAINING cap (opts.MaxResults minus already-collected
			// matches) so ProcessAllChunks can cooperatively cancel as
			// soon as this file alone contributes enough to close the
			// overall budget. remainingResults returns nil when no cap
			// was set at all.
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			chunkResults, perr := ProcessAllChunks(
				ctx, tasks, patternIDs, patternOrder,
				opts.RgExtraArgs, opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if perr != nil && !errors.Is(perr, context.Canceled) {
				// A pattern that does not compile dooms every file, so it
				// travels up instead of turning into a skipped file.
				if errors.Is(perr, ErrInvalidPattern) {
					return nil, perr
				}
				skipped = append(skipped, b.path)
				continue
			}
			// Turn ripgrep's chunk-relative line numbers into file
			// absolute ones. Chunks are newline-aligned, so the
			// newlines counted while feeding the chunks before this one
			// are exactly the lines that precede it: chunk 0 starts at
			// line 1, and every later chunk starts one line after its
			// predecessor's last. A chunk canceled part-way through
			// (the cap fired) counted only part of its newlines, so
			// every chunk after it reports an unknown line number
			// rather than a wrong one.
			startLine := 1
			numbered := true
			for _, res := range chunkResults {
				for _, rm := range res.Matches {
					absLine := -1
					if numbered {
						absLine = startLine + rm.LineNumber - 1
					}
					matchedIDs := IdentifyMatchingPatterns(
						rm.LineText, rm.Submatches,
						patternIDs, patternOrder, opts.RgExtraArgs,
					)
					for _, pid := range matchedIDs {
						m := toMatch(pid, fileID, rm)
						m.AbsoluteLineNumber = absLine
						if absLine > 0 {
							// rx-python reports the absolute number in
							// both fields once it knows it.
							m.RelativeLineNumber = ptrInt(absLine)
						}
						allMatches = append(allMatches, m)
						if cacheEntry != nil {
							cacheEntry.Matches = append(cacheEntry.Matches, m)
						}
						opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
							Pattern: patternIDs[pid], Offset: m.Offset,
							LineNumber: int64(*m.RelativeLineNumber),
						})
					}
				}
				for _, rc := range res.Contexts {
					lineNum, absLine := rc.LineNumber, -1
					if numbered {
						absLine = startLine + rc.LineNumber - 1
						lineNum = absLine
					}
					allContexts = append(allContexts, contextWithFile{
						fileID: fileID,
						ctx: rxtypes.ContextLine{
							RelativeLineNumber: lineNum,
							AbsoluteLineNumber: absLine,
							LineText:           rc.LineText,
							AbsoluteOffset:     rc.Offset,
						},
					})
				}
				if !res.Complete {
					numbered = false
				}
				startLine += int(res.Newlines)
			}
			if opts.afterScan != nil {
				opts.afterScan(b.path)
			}
			// A chunk cut short leaves numbered false; only a scan that
			// read every chunk describes the whole planned file.
			if cacheEntry != nil && numbered {
				toCache[b.path] = cacheEntry
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
				if errors.Is(cerr, ErrInvalidPattern) {
					return nil, cerr
				}
				// A stream that ended early still yielded real matches,
				// so the file is named as incomplete but its matches are
				// kept. Any other error means nothing was read.
				skipped = append(skipped, b.path)
				if !errors.Is(cerr, ErrIncompleteStream) {
					continue
				}
			}
			// The whole file goes through one ripgrep, so the line
			// numbers it reports are the file's own. Reporting them as
			// unknown made a search of a .gz look less informative than
			// the same search of the text inside it.
			for _, rm := range rawMatches {
				matchedIDs := IdentifyMatchingPatterns(
					rm.LineText, rm.Submatches,
					patternIDs, patternOrder, opts.RgExtraArgs,
				)
				for _, pid := range matchedIDs {
					m := toMatch(pid, fileID, rm)
					if rm.LineNumber >= 1 {
						m.AbsoluteLineNumber = rm.LineNumber
					}
					allMatches = append(allMatches, m)
					opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
						Pattern: patternIDs[pid], Offset: m.Offset,
						LineNumber: int64(*m.RelativeLineNumber),
					})
				}
			}
			for _, rc := range rawContexts {
				absLine := -1
				if rc.LineNumber >= 1 {
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
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size,
				countMatchesForFile(allMatches, fileID))
		case "seekable":
			var cacheEntry *ScannedFile
			if b.info != nil {
				cacheEntry = scanToCache(opts, b.path, b.info, "zstd-seekable")
			}
			if cacheEntry != nil {
				// The frame count, set when the file was classified.
				cacheEntry.Chunks = fileChunkCounts[fileID]
			}
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			rawMatches, rawContexts, _, serr := ProcessSeekable(
				ctx, b.path,
				patternIDs, patternOrder, opts.RgExtraArgs,
				opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if serr != nil {
				if errors.Is(serr, ErrInvalidPattern) {
					return nil, serr
				}
				skipped = append(skipped, b.path)
				continue
			}
			// Frames carry their own line numbering, which the scan
			// turns into the file's by counting the lines of the frames
			// before each one. A frame the scan never reached leaves
			// its matches unnumbered rather than numbered from the
			// wrong place.
			for _, rm := range rawMatches {
				matchedIDs := IdentifyMatchingPatterns(
					rm.LineText, rm.Submatches,
					patternIDs, patternOrder, opts.RgExtraArgs,
				)
				for _, pid := range matchedIDs {
					m := toMatch(pid, fileID, rm)
					if rm.AbsoluteLine >= 1 {
						m.AbsoluteLineNumber = rm.AbsoluteLine
						m.RelativeLineNumber = ptrInt(rm.AbsoluteLine)
					}
					allMatches = append(allMatches, m)
					if cacheEntry != nil {
						cacheEntry.Matches = append(cacheEntry.Matches, m)
					}
					opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
						Pattern: patternIDs[pid], Offset: m.Offset,
						LineNumber: int64(*m.RelativeLineNumber),
					})
				}
			}
			for _, rc := range rawContexts {
				lineNum, absLine := rc.LineNumber, -1
				if rc.AbsoluteLine >= 1 {
					lineNum, absLine = rc.AbsoluteLine, rc.AbsoluteLine
				}
				allContexts = append(allContexts, contextWithFile{
					fileID: fileID,
					ctx: rxtypes.ContextLine{
						RelativeLineNumber: lineNum,
						AbsoluteLineNumber: absLine,
						LineText:           rc.LineText,
						AbsoluteOffset:     rc.Offset,
					},
				})
			}
			// Without a result cap ProcessSeekable either reads every
			// frame or returns an error, so a scan that got here is
			// complete.
			if cacheEntry != nil {
				toCache[b.path] = cacheEntry
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size,
				countMatchesForFile(allMatches, fileID))
		case "cached-regular", "cached-seekable":
			cachedMatches := b.cachedMatch
			if b.kind == "cached-seekable" {
				if b.cacheInfo == nil {
					continue
				}
				cachedMatches = b.cacheInfo.Matches
			}
			reconstructStart := time.Now()
			reMatches, reContexts, rerr := ReconstructFromCache(ReconstructRequest{
				SourcePath:    b.path,
				Cached:        cachedMatches,
				Patterns:      patterns,
				FileID:        fileID,
				RgExtraArgs:   opts.RgExtraArgs,
				ContextBefore: opts.ContextBefore,
				ContextAfter:  opts.ContextAfter,
				UseIndex:      !opts.NoIndex,
			})
			if rerr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			prometheus.RecordTraceCacheReconstruction(time.Since(reconstructStart))
			allMatches = append(allMatches, reMatches...)
			for _, cl := range reContexts {
				allContexts = append(allContexts, contextWithFile{fileID: fileID, ctx: cl})
			}
			for _, m := range reMatches {
				opts.HookFirer.OnMatch(ctx, b.path, MatchInfo{
					Pattern:    patternIDs[m.Pattern],
					Offset:     m.Offset,
					LineNumber: int64(m.AbsoluteLineNumber),
				})
			}
			fireOnFile(ctx, opts.HookFirer, b.path, fileStart, b.size, len(reMatches))
		}
	}

	// -------------------------------------------------------------------
	// Phase 4: sort, truncate, finalize
	// -------------------------------------------------------------------
	// A canceled chunk leaves the chunks after it without a line
	// number. That only happens when a cap fired, so the offsets left
	// over are few; an existing index answers them from the nearest
	// checkpoint. Files with no index keep the unknown marker rather
	// than paying for a full pass.
	resolveUnknownLineNumbers(fileIDs, allMatches, allContexts)

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
	// Phase 5: write caches for large completed scans
	// -------------------------------------------------------------------
	// toCache only holds scans started without a result cap, so a
	// capped answer never reaches the cache.
	for _, scan := range toCache {
		SaveScannedFile(*scan, patterns, opts.RgExtraArgs)
	}

	// -------------------------------------------------------------------
	// Phase 6: response assembly
	// -------------------------------------------------------------------
	elapsed := time.Since(start)
	// gated helper — no-op in CLI mode.
	prometheus.AddMatchesFound(len(allMatches))

	// All nullable slice fields must serialize as [] (not null) when
	// empty — the JSON contract says they're arrays. nil slices
	// marshal as null in Go, breaking frontend iteration expectations.
	// Centralize via emptyIfNilStrings for consistency. .
	//
	resp := &rxtypes.TraceResponse{
		RequestID:    opts.RequestID,
		Path:         emptyIfNilStrings(append([]string(nil), paths...)),
		Patterns:     patternIDs,
		Files:        fileIDs,
		Matches:      emptyIfNilMatches(allMatches),
		ScannedFiles: emptyIfNilStrings(scannedFilesOutput(filePaths, scannedDirs)),
		SkippedFiles: emptyIfNilStrings(dedupStrings(skipped)),
		MaxResults:   opts.MaxResults,
		Time:         elapsed.Seconds(),
		// Both maps are built with make, so an empty one marshals as
		// `{}`: the schema declares them as non-null objects, and a
		// client generated from it rejects a `null`.
		FileChunks:   fileChunkCounts,
		ContextLines: contextDict,
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
// recursive defaults to TRUE (Python
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
				if entry.IsDir() || sandbox.SkipEntry(entry.Name()) {
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
		// Hidden entries are skipped before the recursion, so a hidden
		// directory is not descended into either.
		if sandbox.SkipEntry(entry.Name()) {
			continue
		}
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
	width := contextBefore
	if contextAfter > width {
		width = contextAfter
	}

	// Index the context lines by file and line number. Walking the
	// whole slice per match is quadratic, and a large scan produces
	// tens of thousands of both.
	type lineKey struct {
		fileID string
		line   int
	}
	byLine := make(map[lineKey]rxtypes.ContextLine, len(contexts))
	if width > 0 {
		for _, cwf := range contexts {
			byLine[lineKey{fileID: cwf.fileID, line: cwf.ctx.RelativeLineNumber}] = cwf.ctx
		}
	}

	for _, m := range matches {
		if m.RelativeLineNumber == nil {
			continue
		}
		matchLine := *m.RelativeLineNumber
		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)

		// The window is the matched line plus every context line within
		// [-contextBefore, +contextAfter] of it in the SAME file.
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
		for d := -width; d <= width; d++ {
			if d == 0 {
				continue // matched line already added
			}
			if cl, ok := byLine[lineKey{fileID: m.File, line: matchLine + d}]; ok {
				window = append(window, cl)
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

// scanToCache decides, before a file is scanned, whether the scan's
// answer is to be written to the trace cache, and if so starts the
// record with the file's identity as info saw it. It returns nil when
// the answer is not to be cached: the cache is off, a result cap is
// set, or the file is below the size threshold for its kind.
//
// Taking the identity here, before the scan, is what keeps a growing
// log from being cached as larger than the part that was scanned.
// compressionFormat is empty for a plain file and "zstd-seekable" for a
// seekable-zstd one, whose lower threshold ShouldCache applies.
func scanToCache(opts Options, path string, info os.FileInfo, compressionFormat string) *ScannedFile {
	compressed := compressionFormat != ""
	if opts.NoCache {
		return nil
	}
	if !ShouldCache(info.Size(), opts.MaxResults, true, compressed) {
		// The cache is on but this scan does not qualify: too small for
		// its kind, or capped.
		prometheus.RecordTraceCacheSkip()
		return nil
	}
	return &ScannedFile{
		Path:              path,
		Source:            index.IdentityFromInfo(path, info),
		Matches:           []rxtypes.Match{},
		CompressionFormat: compressionFormat,
	}
}
