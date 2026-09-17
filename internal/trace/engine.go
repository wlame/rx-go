package trace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/wlame/rx-go/internal/compression"
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

	// beforeRead, when set, runs once after every file has been checked
	// and classified and before the first byte of any is read. It is a
	// test seam, unexported like afterScan: a test uses it to retarget
	// a link or replace a file at the last moment before the read.
	beforeRead func()
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
// RunWithOptions call manages its own subprocesses and has no shared state beyond
// the immutable config.
type Engine struct{}

// New returns a zero-value Engine. There's no meaningful construction
// for now; kept as a constructor so future additions (e.g. a metrics
// registry injected for tests) don't break callers.
func New() *Engine { return &Engine{} }

// RunWithOptions is the top-level search, used by the CLI, the HTTP
// handler and tests. It:
//
//  1. Resolves each input path (file vs. directory, validated).
//  2. Assigns file IDs ("f1", "f2", ...) and pattern IDs ("p1", ...).
//  3. Classifies each file into one of four buckets:
//     a. Regular        — chunked + parallel ProcessChunk
//     b. Compressed     — ProcessCompressed (single-stream)
//     c. Seekable zstd  — ProcessSeekable (frame-parallel)
//     d. Cache hit      — reconstruct from disk (ReconstructMatchData)
//  4. Runs each bucket through its path, collects MatchRaw.
//  5. Decides which patterns match each matched line (creditPatterns)
//     and reports the line once per pattern, with its own submatches.
//  6. Sorts, truncates to max_results, resolves absolute line numbers
//     (for chunked files that lack them), and builds a TraceResponse.
//     OnFile fires as each file's scan finishes; OnMatch fires once per
//     match of the result, after this step, with the result's numbers.
//  7. Optionally writes new trace caches for large completed scans.
//  8. Returns the response.
//
// Parity with rx-python/src/rx/trace.py::parse_paths is bit-for-bit
// up to intentional differences in logging (structured vs slog) and
// the order in which concurrent batches complete (we sort at the end,
// same as Python).
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
	files, scannedDirs, skipped := expandPaths(paths, !opts.NoRecursive)
	if len(files) == 0 {
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
	// Each file is reported under the caller's spelling of its path, and
	// read only through its pin (sources), which refuses a path that no
	// longer leads to the file expandPaths checked.
	filePaths := make([]string, len(files))
	fileIDs := make(map[string]string, len(files))
	filePathToID := make(map[string]string, len(files))
	sources := make(map[string]sandbox.Pinned, len(files))
	for i, src := range files {
		id := "f" + strconv.Itoa(i+1)
		filePaths[i] = src.Path()
		fileIDs[id] = src.Path()
		filePathToID[src.Path()] = id
		sources[id] = src
	}

	// -------------------------------------------------------------------
	// Phase 2: classify each file
	// -------------------------------------------------------------------
	type fileBucket struct {
		kind string // "regular" | "compressed" | "seekable" | "cached-regular" | "cached-seekable"
		path string
		// src is the file as expandPaths checked it. Every read of the
		// file goes through it.
		src  sandbox.Pinned
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
		src := sources[filePathToID[fp]]
		// The current stat, for the size the scan plans from; Stat
		// refuses it when the path no longer leads to the checked file.
		fi, err := src.Stat()
		if errors.Is(err, sandbox.ErrFileChanged) {
			skipped = append(skipped, fp)
			continue
		}
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
						kind: "cached-seekable", path: fp, src: src, size: sz, info: fi, cacheInfo: info,
					})
					fileChunkCounts[filePathToID[fp]] = info.ChunkCount
					continue
				}
			}
			buckets = append(buckets, fileBucket{kind: "seekable", path: fp, src: src, size: sz, info: fi})
			// Chunk count for seekable = frame count. We fetch it via
			// the seek table; failure falls back to 1.
			if tbl, terr := readSeekTable(src); terr == nil {
				fileChunkCounts[filePathToID[fp]] = tbl.NumFrames
			} else {
				fileChunkCounts[filePathToID[fp]] = 1
			}
			continue
		}
		if compression.IsCompressed(fp) {
			buckets = append(buckets, fileBucket{kind: "compressed", path: fp, src: src, size: sz, info: fi})
			fileChunkCounts[filePathToID[fp]] = 1
			continue
		}
		// Regular files — try cache if large enough.
		if !opts.NoCache && sz >= largeFileThresholdBytes() {
			if cached, cerr := GetCachedScan(fp, patterns, opts.RgExtraArgs); cerr == nil {
				buckets = append(buckets, fileBucket{
					kind: "cached-regular", path: fp, src: src, size: sz, info: fi, cachedMatch: cached.Matches,
				})
				// The chunk count of the scan that wrote the cache, so a
				// cache hit answers exactly what that scan answered.
				fileChunkCounts[filePathToID[fp]] = cached.ChunkCount
				continue
			}
		}
		buckets = append(buckets, fileBucket{kind: "regular", path: fp, src: src, size: sz, info: fi})
	}

	// -------------------------------------------------------------------
	// Phase 3: execute each bucket
	// -------------------------------------------------------------------
	if opts.beforeRead != nil {
		opts.beforeRead()
	}
	var allMatches []rxtypes.Match
	var allContexts []contextWithFile
	// Where every line above ends, by where it starts. The context
	// windows are put together from it (see buildContextDict).
	ends := lineEnds{}
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
			tasks, terr := planFileTasks(b.src, b.info.Size())
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
			credits, cerr := creditPatterns(ctx, creditRequest{
				source: b.src, lines: allChunkMatches(chunkResults),
				patternIDs: patternIDs, patternOrder: patternOrder, rgExtraArgs: opts.RgExtraArgs,
			})
			if cerr != nil {
				if fatal := creditFailureEndsTrace(ctx, cerr); fatal != nil {
					return nil, fatal
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
			line := 0 // index of rm in credits, which lists every chunk's matches in turn
			for _, res := range chunkResults {
				for _, rm := range res.Matches {
					ends.record(fileID, rm.Offset, rm.End)
					absLine := -1
					if numbered {
						absLine = startLine + rm.LineNumber - 1
					}
					for _, credit := range credits[line] {
						m := toMatch(credit, fileID, rm)
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
					}
					line++
				}
				for _, rc := range res.Contexts {
					ends.record(fileID, rc.Offset, rc.End)
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
							LineTextTruncated:  rc.LineTextTruncated,
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
			if cacheEntry != nil && numbered && !chunksCutALine(chunkResults) {
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
				ctx, b.src, format,
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
			credits, crErr := creditPatterns(ctx, creditRequest{
				source: b.src, lines: rawMatches,
				patternIDs: patternIDs, patternOrder: patternOrder, rgExtraArgs: opts.RgExtraArgs,
			})
			if crErr != nil {
				if fatal := creditFailureEndsTrace(ctx, crErr); fatal != nil {
					return nil, fatal
				}
				skipped = append(skipped, b.path)
				continue
			}
			// The whole file goes through one ripgrep, so the line
			// numbers it reports are the file's own. Reporting them as
			// unknown made a search of a .gz look less informative than
			// the same search of the text inside it.
			for i, rm := range rawMatches {
				ends.record(fileID, rm.Offset, rm.End)
				for _, credit := range credits[i] {
					m := toMatch(credit, fileID, rm)
					if rm.LineNumber >= 1 {
						m.AbsoluteLineNumber = rm.LineNumber
					}
					allMatches = append(allMatches, m)
				}
			}
			for _, rc := range rawContexts {
				ends.record(fileID, rc.Offset, rc.End)
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
						LineTextTruncated:  rc.LineTextTruncated,
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
				cacheEntry.FrameIndexByOffset = map[int64]int{}
			}
			remaining := remainingResults(opts.MaxResults, len(allMatches))
			rawMatches, rawContexts, _, serr := ProcessSeekable(
				ctx, b.src,
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
			credits, crErr := creditPatterns(ctx, creditRequest{
				source: b.src, lines: rawMatches,
				patternIDs: patternIDs, patternOrder: patternOrder, rgExtraArgs: opts.RgExtraArgs,
			})
			if crErr != nil {
				if fatal := creditFailureEndsTrace(ctx, crErr); fatal != nil {
					return nil, fatal
				}
				skipped = append(skipped, b.path)
				continue
			}
			// Frames carry their own line numbering, which the scan
			// turns into the file's by counting the lines of the frames
			// before each one. A frame the scan never reached leaves
			// its matches unnumbered rather than numbered from the
			// wrong place.
			for i, rm := range rawMatches {
				ends.record(fileID, rm.Offset, rm.End)
				for _, credit := range credits[i] {
					m := toMatch(credit, fileID, rm)
					if rm.AbsoluteLine >= 1 {
						m.AbsoluteLineNumber = rm.AbsoluteLine
						m.RelativeLineNumber = ptrInt(rm.AbsoluteLine)
					}
					allMatches = append(allMatches, m)
					if cacheEntry != nil {
						cacheEntry.Matches = append(cacheEntry.Matches, m)
						cacheEntry.FrameIndexByOffset[m.Offset] = rm.FrameIndex
					}
				}
			}
			for _, rc := range rawContexts {
				ends.record(fileID, rc.Offset, rc.End)
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
						LineTextTruncated:  rc.LineTextTruncated,
					},
				})
			}
			// Without a result cap ProcessSeekable either reads every
			// frame or returns an error, so a scan that got here is
			// complete.
			if cacheEntry != nil && !linesCut(rawMatches, rawContexts) {
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
			// Under a cap only the first max_results matches of this file
			// by offset can survive the engine's cut, so the pass stops
			// after them; without one it rebuilds every cached match.
			maxMatches := 0
			if opts.MaxResults != nil {
				maxMatches = *opts.MaxResults
			}
			reMatches, reContexts, reEnds, rerr := reconstructLines(ReconstructRequest{
				Source:        b.src,
				Cached:        cachedMatches,
				Patterns:      patterns,
				FileID:        fileID,
				RgExtraArgs:   opts.RgExtraArgs,
				ContextBefore: opts.ContextBefore,
				ContextAfter:  opts.ContextAfter,
				UseIndex:      !opts.NoIndex,
				MaxMatches:    maxMatches,
			})
			if rerr != nil {
				skipped = append(skipped, b.path)
				continue
			}
			prometheus.RecordTraceCacheReconstruction(time.Since(reconstructStart))
			allMatches = append(allMatches, reMatches...)
			for start, end := range reEnds {
				ends.record(fileID, start, end)
			}
			for _, cl := range reContexts {
				allContexts = append(allContexts, contextWithFile{fileID: fileID, ctx: cl})
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
	resolveUnknownLineNumbers(sources, allMatches, allContexts, lineResolverFor(opts))

	sort.SliceStable(allMatches, func(i, j int) bool {
		if allMatches[i].File != allMatches[j].File {
			return allMatches[i].File < allMatches[j].File
		}
		if allMatches[i].Offset != allMatches[j].Offset {
			return allMatches[i].Offset < allMatches[j].Offset
		}
		return allMatches[i].Pattern < allMatches[j].Pattern
	})
	// Every match found, kept or not, can be a line in a kept match's
	// window, so the windows look lines up in the matches before the cut.
	uncutMatches := allMatches
	if opts.MaxResults != nil && len(allMatches) > *opts.MaxResults {
		allMatches = allMatches[:*opts.MaxResults]
	}
	// Only now is every match numbered as the response numbers it and
	// the set cut to the cap, so the match_found events go out here
	// rather than while the chunks are read.
	fireMatchHooks(ctx, opts.HookFirer, allMatches, fileIDs, patternIDs)

	contextDict := buildContextDict(allMatches, uncutMatches, allContexts, ends, opts.ContextBefore, opts.ContextAfter)

	// -------------------------------------------------------------------
	// Phase 5: write caches for large completed scans
	// -------------------------------------------------------------------
	// toCache only holds scans started without a result cap, so a
	// capped answer never reaches the cache, and only scans whose answer
	// cuts no line (see linesCut).
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
// Every path is pinned (sandbox.Pin): checked against the search roots
// and the hidden rule once more, and tied to the file it leads to now,
// so the scan later reads that file or nothing. A directory is walked
// by sandbox.WalkPinned, which follows a symlink only when naming its
// target would be allowed, enters each directory once, and pins every
// file it reports. A path or entry it refuses goes into `skipped`, as do
// binary and unreadable files. Compressed archives (gzip/xz/bz2/zst) are
// treated as text because ripgrep can read them via decompressors.
func expandPaths(paths []string, recursive bool) (files []sandbox.Pinned, scannedDirs, skipped []string) {
	for _, p := range paths {
		src, err := sandbox.Pin(p)
		if err != nil {
			skipped = append(skipped, p)
			continue
		}
		if !src.Info().IsDir() {
			if !isTextFile(src) {
				skipped = append(skipped, p)
				continue
			}
			files = append(files, src)
			continue
		}

		scannedDirs = append(scannedDirs, p)
		entries, walkErr := sandbox.WalkPinned(src, recursive)
		if walkErr != nil {
			// The directory itself cannot be listed: report it rather
			// than answer as if it were empty.
			skipped = append(skipped, p)
			continue
		}
		for _, entry := range entries {
			switch {
			case entry.ReadErr != nil:
				// A subdirectory that cannot be listed (permission
				// denied) is passed over; the rest of the tree is
				// still searched.
				continue
			case entry.Refused != "", !isTextFile(entry.File):
				skipped = append(skipped, entry.Path)
			default:
				files = append(files, entry.File)
			}
		}
	}
	return files, scannedDirs, skipped
}

// isTextFile returns true when the first 8 KB of the file contains no
// null bytes. Mirrors Python's is_text_file in file_utils.py — we
// special-case compressed files as "text" since ripgrep (via decompressor)
// will read them.
func isTextFile(src sandbox.Pinned) bool {
	if compression.IsCompressed(src.Path()) {
		return true
	}
	f, err := src.Open()
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

// chunksCutALine reports whether any line the chunks returned, matched
// or context, holds only part of its text or submatches.
func chunksCutALine(results []ChunkResult) bool {
	for _, res := range results {
		if linesCut(res.Matches, res.Contexts) {
			return true
		}
	}
	return false
}

// linesCut reports whether any of the lines holds only part of its text
// or submatches.
//
// Such a scan is not written to the trace cache. A cache hit rebuilds
// each line from the file and its submatches from the text it keeps, so
// a submatch that runs past a cut would come back ending at the cut,
// where the scan reports its true end: a hit would answer differently.
// The scan is repeated instead.
func linesCut(matches []MatchRaw, contexts []ContextRaw) bool {
	for _, m := range matches {
		if m.truncated() {
			return true
		}
	}
	for _, c := range contexts {
		if c.LineTextTruncated {
			return true
		}
	}
	return false
}

// toMatch turns a matched line and one pattern it is credited to into
// the final rxtypes.Match: the line's place and text, and that pattern's
// own submatches. Wraps pointer-conversion for line number + line text.
func toMatch(credit patternCredit, fileID string, rm MatchRaw) rxtypes.Match {
	line := rm.LineNumber
	text := rm.LineText
	return rxtypes.Match{
		Pattern:             credit.patternID,
		File:                fileID,
		Offset:              rm.Offset,
		RelativeLineNumber:  &line,
		AbsoluteLineNumber:  -1, // resolved later if possible
		LineText:            &text,
		Submatches:          credit.submatches,
		LineTextTruncated:   rm.LineTextTruncated,
		SubmatchesTruncated: credit.submatchesTruncated,
	}
}

// allChunkMatches lists the matches of every chunk, chunk after chunk.
func allChunkMatches(results []ChunkResult) []MatchRaw {
	var out []MatchRaw
	for _, res := range results {
		out = append(out, res.Matches...)
	}
	return out
}

// creditFailureEndsTrace returns the error that ends the whole trace
// when deciding a file's patterns failed with err, and nil when only
// that file is lost (it is then reported as skipped, like a file whose
// scan failed). A pattern ripgrep refuses dooms every file, and a
// canceled request stops the trace.
func creditFailureEndsTrace(ctx context.Context, err error) error {
	if errors.Is(err, ErrInvalidPattern) {
		return err
	}
	return ctx.Err()
}

// fireMatchHooks calls OnMatch once for each match of the answer, in
// the answer's order. The engine calls it after the matches are
// numbered, sorted and cut to the cap, so the webhook reports exactly
// the matches the response holds, and each event's line number is the
// response's absolute_line_number: the line's number in the file, or -1
// where a capped scan could not count it. relative_line_number is never
// sent, because a cut-short scan leaves it counted from the start of a
// chunk. The offset always goes with it, so a receiver can resolve an
// unknown line with `rx samples --offsets=…`.
//
// files and patterns are the response's ID maps ("f1" → path,
// "p1" → pattern).
func fireMatchHooks(
	ctx context.Context,
	firer HookFirer,
	matches []rxtypes.Match,
	files, patterns map[string]string,
) {
	for _, m := range matches {
		firer.OnMatch(ctx, files[m.File], MatchInfo{
			Pattern:    patterns[m.Pattern],
			Offset:     m.Offset,
			LineNumber: int64(m.AbsoluteLineNumber),
		})
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

// lineAt names one line of one file by the offset of its first byte,
// which no other line of the file shares.
type lineAt struct {
	fileID string
	offset int64
}

// lineEnds records where each line a scan reported ends, by where it
// starts: the offset one past the line's last byte, its line break
// included, which is where the next line starts. Every scan path knows
// it exactly (ripgrep reports each line whole, and the cache-hit pass
// reads the file), and it is what links a line to its neighbors.
type lineEnds map[lineAt]int64

// record notes that the line of fileID starting at start ends at end.
// A line holds at least its line break or one character, so an end
// that is not past the start is no end at all and is not recorded.
func (e lineEnds) record(fileID string, start, end int64) {
	if end > start {
		e[lineAt{fileID: fileID, offset: start}] = end
	}
}

// buildContextDict groups context lines around each match in matches,
// keyed by "<pattern>:<file>:<offset>".
//
// A match's window is its own line plus the contextBefore lines just
// before it and the contextAfter lines just after it in the same file,
// each bound applied on its own: `-B 12 -A 1` gives at most 12 lines
// before and 1 after. The lines come from two places. ripgrep reports
// the lines around a match as context, and a line in the window that
// matches too as a match of its own; a line that several patterns
// match appears once. The matched lines come from found, every match
// the scan found, which a result cap may have cut matches from: a
// match past the cap is still a line of the window of the last match
// kept.
//
// INVARIANT: a window holds only lines next to its match in the file.
// The window is walked line by line through byte offsets: the line
// before a line is the one that ends where it starts, and the line
// after it starts where it ends. Line numbers play no part, because a
// scan a cap cut short leaves some lines numbered from the start of
// their chunk or frame, and such a number names a different line in
// every chunk. The walk stops at the first line no scan reported, so a
// window can come out short but never holds a line from elsewhere.
func buildContextDict(
	matches []rxtypes.Match,
	found []rxtypes.Match,
	contexts []contextWithFile,
	ends lineEnds,
	contextBefore, contextAfter int,
) map[string][]rxtypes.ContextLine {
	out := make(map[string][]rxtypes.ContextLine)
	lines := windowLines{byStart: map[lineAt]rxtypes.ContextLine{}, startOfLineEndingAt: map[lineAt]int64{}}
	if contextBefore > 0 || contextAfter > 0 {
		for _, m := range found {
			if m.RelativeLineNumber != nil {
				lines.add(lineAt{fileID: m.File, offset: m.Offset}, matchedLineAsContext(m), ends)
			}
		}
		for _, cwf := range contexts {
			lines.add(lineAt{fileID: cwf.fileID, offset: cwf.ctx.AbsoluteOffset}, cwf.ctx, ends)
		}
	}

	for _, m := range matches {
		if m.RelativeLineNumber == nil {
			continue
		}
		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)
		matched := lineAt{fileID: m.File, offset: m.Offset}

		window := []rxtypes.ContextLine{matchedLineAsContext(m)}
		for at, i := matched, 0; i < contextBefore; i++ {
			prev, ok := lines.before(at)
			if !ok {
				break
			}
			window = append(window, lines.byStart[prev])
			at = prev
		}
		for at, i := matched, 0; i < contextAfter; i++ {
			next, ok := lines.after(at, ends)
			if !ok {
				break
			}
			window = append(window, lines.byStart[next])
			at = next
		}
		sort.Slice(window, func(i, j int) bool {
			return window[i].AbsoluteOffset < window[j].AbsoluteOffset
		})
		out[key] = window
	}
	return out
}

// windowLines holds every line a context window can take, by where it
// starts, and the reverse link from where a line ends back to where it
// starts.
type windowLines struct {
	byStart             map[lineAt]rxtypes.ContextLine
	startOfLineEndingAt map[lineAt]int64
}

// add records line, which starts at at, unless a line already recorded
// there says as much. Two scans can both report a line, one of them
// with its number and the other, cut short by a cap, without it; the
// numbered copy is kept. A line whose end is unknown cannot be linked
// to its neighbors and is left out.
func (w windowLines) add(at lineAt, line rxtypes.ContextLine, ends lineEnds) {
	end, known := ends[at]
	if !known {
		return
	}
	if have, seen := w.byStart[at]; seen && (have.AbsoluteLineNumber >= 1 || line.AbsoluteLineNumber < 1) {
		return
	}
	w.byStart[at] = line
	w.startOfLineEndingAt[lineAt{fileID: at.fileID, offset: end}] = at.offset
}

// before returns the line just before the line starting at at: the one
// that ends where it starts.
func (w windowLines) before(at lineAt) (lineAt, bool) {
	start, ok := w.startOfLineEndingAt[at]
	return lineAt{fileID: at.fileID, offset: start}, ok
}

// after returns the line just after the line starting at at: the one
// that starts where it ends.
func (w windowLines) after(at lineAt, ends lineEnds) (lineAt, bool) {
	end, ok := ends[at]
	if !ok {
		return lineAt{}, false
	}
	next := lineAt{fileID: at.fileID, offset: end}
	_, reported := w.byStart[next]
	return next, reported
}

// matchedLineAsContext is a match's line in the form a window lists it.
// The caller has checked that RelativeLineNumber is set.
func matchedLineAsContext(m rxtypes.Match) rxtypes.ContextLine {
	text := ""
	if m.LineText != nil {
		text = *m.LineText
	}
	return rxtypes.ContextLine{
		RelativeLineNumber: *m.RelativeLineNumber,
		AbsoluteLineNumber: m.AbsoluteLineNumber,
		LineText:           text,
		AbsoluteOffset:     m.Offset,
		LineTextTruncated:  m.LineTextTruncated,
	}
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
