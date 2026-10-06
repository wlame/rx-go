package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
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
	// UncachedPaths are files, spelled as in the paths given to
	// RunWithOptions, whose trace cache is neither read nor written,
	// whatever NoCache says. The CLI lists the temporary file it spools
	// piped input to: the file is deleted when the command ends, so an
	// entry for it could never be read again.
	UncachedPaths []string
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

// usesTraceCache reports whether the trace cache is read and written
// for the file at path.
func (o *Options) usesTraceCache(path string) bool {
	return !o.NoCache && !slices.Contains(o.UncachedPaths, path)
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
//  1. Assigns pattern IDs ("p1", ...) and checks that rg compiles the
//     patterns with the request's flags (validatePatterns), then
//     resolves each input path (file vs. directory, validated).
//  2. Assigns file IDs ("f1", "f2", ...).
//  3. Classifies each file into one of four buckets:
//     a. Regular        — chunked + parallel ProcessChunk
//     b. Compressed     — ProcessCompressed (single-stream)
//     c. Seekable zstd  — ProcessSeekable (frame-parallel)
//     d. Cache hit      — reconstruct from disk (ReconstructMatchData)
//  4. Runs each bucket through its path, collects MatchRaw.
//  5. Decides which patterns match each matched line, for every file at
//     once (settleFiles, creditPatterns), and reports the line once per
//     pattern, with its own submatches. OnFile fires then for each file
//     read, in order, with its match count.
//  6. Sorts, truncates to max_results, resolves absolute line numbers
//     (for chunked files that lack them), and builds a TraceResponse.
//     OnMatch fires once per match of the result, after this step, with
//     the result's numbers.
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
	// Pattern IDs are the caller's patterns in order: p1, p2, ...
	patternIDs := patternIDsMap(patterns)
	patternOrder := make([]string, 0, len(patterns))
	for i := range patterns {
		patternOrder = append(patternOrder, "p"+strconv.Itoa(i+1))
	}
	// A pattern rg cannot compile fails the request here, before any
	// path is walked or file read, the way rg itself compiles before it
	// searches. Every later rg run classifies a pattern error the same
	// way (ripgrepExitError), so none can pass for a file error.
	if err := validatePatterns(ctx, patternIDs, patternOrder, opts.RgExtraArgs); err != nil {
		return nil, err
	}
	// skips collects every path passed over or not searched in full,
	// with the reason the answer gives for it (skip_reasons).
	files, scannedDirs, skips := expandPaths(paths, !opts.NoRecursive)
	if len(files) == 0 {
		// Nothing to search still answers with the request it was
		// given: an empty request id and a null path made a skipped
		// file indistinguishable from a malformed response.
		return &rxtypes.TraceResponse{
			RequestID:    opts.RequestID,
			Path:         emptyIfNilStrings(append([]string(nil), paths...)),
			Patterns:     patternIDs,
			Files:        map[string]string{},
			Matches:      []rxtypes.Match{},
			ScannedFiles: []string{},
			// emptyIfNilStrings coerces a nil slice to []string{} so
			// the JSON marshaller emits `[]` instead of `null`. Python
			// emits `[]` for empty lists.
			SkippedFiles: skips.paths(),
			SkipReasons:  skips.reasons(),
			MaxResults:   opts.MaxResults,
			Time:         time.Since(start).Seconds(),
			// Empty objects rather than null, as on the main path.
			FileChunks:   map[string]int{},
			ContextLines: map[string][]rxtypes.ContextLine{},
		}, nil
	}

	// -------------------------------------------------------------------
	// Phase 1: assign file IDs
	// -------------------------------------------------------------------
	// Each file is reported under the caller's spelling of its path, and
	// read only through its pin (sources), which refuses a path that no
	// longer leads to the file expandPaths checked.
	filePaths := make([]string, len(files))
	fileIDs := make(map[string]string, len(files))
	filePathToID := make(map[string]string, len(files))
	sources := make(map[string]sandbox.Pinned, len(files))
	// What each file is (filekind), decided once by expandPaths from
	// the file's own bytes.
	kinds := make(map[string]filekind.Kind, len(files))
	for i, file := range files {
		id := "f" + strconv.Itoa(i+1)
		filePaths[i] = file.src.Path()
		fileIDs[id] = file.src.Path()
		filePathToID[file.src.Path()] = id
		sources[id] = file.src
		kinds[id] = file.kind
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
		// table is a seekable file's seek table, read and checked
		// against the file when the file was classified.
		table *seekable.SeekTable
		// format is a compressed file's stream format.
		format compression.Format
		// statErr is why the stat taken at classification failed, when
		// info is nil.
		statErr error
	}
	var buckets []fileBucket
	fileChunkCounts := make(map[string]int)

	for _, fp := range filePaths {
		src := sources[filePathToID[fp]]
		// The current stat, for the size the scan plans from; Stat
		// refuses it when the path no longer leads to the checked file.
		fi, err := src.Stat()
		if errors.Is(err, sandbox.ErrFileChanged) {
			skips.addErr(fp, err)
			continue
		}
		var sz int64
		statErr := err
		if err == nil {
			sz = fi.Size()
		} else {
			fi = nil
		}

		kind := kinds[filePathToID[fp]]
		if kind.TableMismatch != nil {
			slog.Default().Warn("seek_table_mismatch", "path", fp, "error", kind.TableMismatch.Error(),
				"read_as", "plain zstd")
		}
		if kind.TableUnused != nil {
			// The same text, one stream instead of frames in parallel:
			// slower, which an operator may want to know about.
			slog.Default().Warn("seek_table_unused", "path", fp, "error", kind.TableUnused.Error(),
				"read_as", "plain zstd")
		}
		// Seekable zstd: a zstd file whose seek table describes it. One
		// whose table does not is read as the plain zstd stream it still
		// is, below.
		if kind.IsSeekable() {
			tbl := kind.Table
			if opts.usesTraceCache(fp) {
				if info, cerr := GetCompressedCacheInfo(fp, patterns, opts.RgExtraArgs); cerr == nil {
					buckets = append(buckets, fileBucket{
						kind: "cached-seekable", path: fp, src: src, size: sz, info: fi, cacheInfo: info,
					})
					fileChunkCounts[filePathToID[fp]] = info.ChunkCount
					continue
				}
			}
			buckets = append(buckets, fileBucket{kind: "seekable", path: fp, src: src, size: sz, info: fi, table: tbl})
			// Chunk count for seekable = frame count.
			fileChunkCounts[filePathToID[fp]] = tbl.NumFrames
			continue
		}
		if kind.IsCompressed() {
			buckets = append(buckets, fileBucket{
				kind: "compressed", path: fp, src: src, size: sz, info: fi, format: kind.Format,
			})
			fileChunkCounts[filePathToID[fp]] = 1
			continue
		}
		// Regular files — try cache if large enough.
		if opts.usesTraceCache(fp) && sz >= largeFileThresholdBytes() {
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
		buckets = append(buckets, fileBucket{kind: "regular", path: fp, src: src, size: sz, info: fi, statErr: statErr})
	}

	// -------------------------------------------------------------------
	// Phase 3: execute each bucket
	// -------------------------------------------------------------------
	if opts.beforeRead != nil {
		opts.beforeRead()
	}
	var allContexts []contextWithFile
	// Where every line above ends, by where it starts. The context
	// windows are put together from it (see buildContextDict).
	ends := lineEnds{}
	// What each file read gave, in bucket order, until it is settled. A
	// scanned file's lines wait there for their patterns, which are
	// decided for several files at once (see settleFiles and
	// settleWhenDue).
	var outcomes []fileOutcome
	pendingText := 0 // bytes of matched line text in outcomes
	var allMatches []rxtypes.Match
	// Completed scans whose answer goes into the trace cache, keyed by
	// path (see scannedLines.cacheable).
	toCache := map[string]*ScannedFile{}
	settle := func() error {
		matches, cacheable, lost, err := settleFiles(ctx, opts, outcomes, patternIDs, patternOrder)
		if err != nil {
			return err
		}
		allMatches = append(allMatches, matches...)
		for path, entry := range cacheable {
			toCache[path] = entry
		}
		skips.addAll(lost)
		outcomes, pendingText = nil, 0
		return nil
	}
	// found counts the matched lines read so far, a cache hit's matches
	// included. Each line is at least one match, so the cap is reached no
	// later than it would be on the matches themselves.
	found := 0

	for _, b := range buckets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// Stop early once max_results is hit.
		if opts.MaxResults != nil && found >= *opts.MaxResults {
			break
		}
		fileID := filePathToID[b.path]
		fileStart := time.Now()
		if b.info != nil {
			// gated helper — no-op in CLI mode.
			prometheus.RecordFileScanned(b.size)
		}
		outcome := fileOutcome{fileID: fileID, path: b.path, size: b.size}

		switch b.kind {
		case "regular":
			if b.info == nil {
				skips.addErr(b.path, b.statErr)
				continue
			}
			// Plan from the stat taken at classification and record that
			// stat as the file's identity, before any byte is read.
			tasks, terr := planFileTasks(b.src, b.info.Size())
			if terr != nil {
				skips.addErr(b.path, terr)
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
			remaining := remainingResults(opts.MaxResults, found)
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
				skips.addErr(b.path, perr)
				continue
			}
			scan := &scannedLines{source: b.src, cacheEntry: cacheEntry}
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
					ends.record(fileID, rm.Offset, rm.End)
					m := toMatch(patternCredit{}, fileID, rm)
					if numbered {
						m.AbsoluteLineNumber = startLine + rm.LineNumber - 1
						// rx-python reports the absolute number in both
						// fields once it knows it.
						m.RelativeLineNumber = ptrInt(m.AbsoluteLineNumber)
					}
					scan.add(rm, m)
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
			scan.cacheable = cacheEntry != nil && numbered && !chunksCutALine(chunkResults)
			outcome.scanned = scan
		case "compressed":
			remaining := remainingResults(opts.MaxResults, found)
			rawMatches, rawContexts, _, cerr := ProcessCompressed(
				ctx, b.src, b.format,
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
				skips.addErr(b.path, cerr)
				if !errors.Is(cerr, ErrIncompleteStream) {
					continue
				}
			}
			scan := &scannedLines{source: b.src}
			// The whole file goes through one ripgrep, so the line
			// numbers it reports are the file's own. Reporting them as
			// unknown made a search of a .gz look less informative than
			// the same search of the text inside it.
			for _, rm := range rawMatches {
				ends.record(fileID, rm.Offset, rm.End)
				m := toMatch(patternCredit{}, fileID, rm)
				if rm.LineNumber >= 1 {
					m.AbsoluteLineNumber = rm.LineNumber
				}
				scan.add(rm, m)
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
			outcome.scanned = scan
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
			remaining := remainingResults(opts.MaxResults, found)
			rawMatches, rawContexts, _, serr := processSeekable(
				ctx, b.src, b.table,
				patternIDs, patternOrder, opts.RgExtraArgs,
				opts.ContextBefore, opts.ContextAfter,
				remaining,
			)
			if serr != nil {
				if errors.Is(serr, ErrInvalidPattern) {
					return nil, serr
				}
				// A damaged frame costs only the lines that touch it, so
				// the matches of every other line are real: the file is
				// named as not searched in full and its matches are kept,
				// as for a gzip stream that ends early. Any other error
				// means nothing read can be trusted.
				skips.addErr(b.path, serr)
				if !errors.Is(serr, seekable.ErrDamagedFrame) {
					continue
				}
			}
			scan := &scannedLines{source: b.src, cacheEntry: cacheEntry}
			// Frames carry their own line numbering, which the scan
			// turns into the file's by counting the lines of the frames
			// before each one. A frame the scan never reached leaves
			// its matches unnumbered rather than numbered from the
			// wrong place.
			for _, rm := range rawMatches {
				ends.record(fileID, rm.Offset, rm.End)
				m := toMatch(patternCredit{}, fileID, rm)
				if rm.AbsoluteLine >= 1 {
					m.AbsoluteLineNumber = rm.AbsoluteLine
					m.RelativeLineNumber = ptrInt(rm.AbsoluteLine)
				}
				scan.add(rm, m)
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
			// frame or returns an error, so a scan that got here without
			// one is complete. A scan around damaged frames is not, and
			// is never cached: the answer would outlive the damage.
			scan.cacheable = serr == nil && cacheEntry != nil && !linesCut(rawMatches, rawContexts)
			outcome.scanned = scan
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
				ContextBefore: opts.ContextBefore,
				ContextAfter:  opts.ContextAfter,
				UseIndex:      !opts.NoIndex,
				MaxMatches:    maxMatches,
			})
			if rerr != nil {
				skips.addErr(b.path, rerr)
				continue
			}
			prometheus.RecordTraceCacheReconstruction(time.Since(reconstructStart))
			outcome.matches = reMatches
			for start, end := range reEnds {
				ends.record(fileID, start, end)
			}
			for _, cl := range reContexts {
				allContexts = append(allContexts, contextWithFile{fileID: fileID, ctx: cl})
			}
		}
		if outcome.scanned != nil {
			found += len(outcome.scanned.lines)
			pendingText += outcome.scanned.text
		} else {
			found += len(outcome.matches)
		}
		outcome.readTime = time.Since(fileStart)
		outcomes = append(outcomes, outcome)
		if settleWhenDue(len(outcomes), pendingText, len(patternOrder)) {
			if err := settle(); err != nil {
				return nil, err
			}
		}
	}
	if err := settle(); err != nil {
		return nil, err
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
		SkippedFiles: skips.paths(),
		SkipReasons:  skips.reasons(),
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

// searchFile is a file a trace reads: its pin and what it is.
type searchFile struct {
	src  sandbox.Pinned
	kind filekind.Kind
}

// expandPaths splits input paths into (files, dirs-scanned, skips).
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
// file it reports. Each file is then classified once (filekind.OfPinned)
// through its pin: what the classification finds decides how the file
// is read. skips names, each with its reason, every path refused, every
// file that cannot be opened, every file whose text is not text (a
// binary file, a .tar.gz, UTF-16) and every subdirectory that cannot be
// listed; the rest of a tree is still searched.
func expandPaths(paths []string, recursive bool) (files []searchFile, scannedDirs []string, skips *skipList) {
	skips = &skipList{}
	for _, p := range paths {
		src, err := sandbox.Pin(p)
		if err != nil {
			skips.addErr(p, err)
			continue
		}
		if !src.Info().IsDir() {
			if file, reason := classifyForSearch(src); reason == "" {
				files = append(files, file)
			} else {
				skips.add(p, reason)
			}
			continue
		}

		scannedDirs = append(scannedDirs, p)
		entries, walkErr := sandbox.WalkPinned(src, recursive)
		if walkErr != nil {
			// The directory itself cannot be listed: report it rather
			// than answer as if it were empty.
			skips.addErr(p, walkErr)
			continue
		}
		for _, entry := range entries {
			switch {
			case entry.ReadErr != nil:
				// A subdirectory that cannot be listed (permission
				// denied) is named with its reason; the rest of the tree
				// is still searched.
				skips.addErr(entry.Path, entry.ReadErr)
			case entry.Refused != "":
				skips.add(entry.Path, entry.Refused)
			default:
				if file, reason := classifyForSearch(entry.File); reason == "" {
					files = append(files, file)
				} else {
					skips.add(entry.Path, reason)
				}
			}
		}
	}
	return files, scannedDirs, skips
}

// classifyForSearch decides what src is through its pin. reason is
// empty for a file the search reads, and otherwise says why it is
// skipped: the file cannot be opened, or its text is not text.
func classifyForSearch(src sandbox.Pinned) (file searchFile, reason string) {
	kind, err := filekind.OfPinnedForReading(src)
	if err != nil {
		return searchFile{}, skipReason(err)
	}
	if !kind.IsText() {
		return searchFile{}, kind.NotText
	}
	return searchFile{src: src, kind: kind}, ""
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

// linesCut reports whether any of the lines holds only part of its text.
//
// Such a scan is not written to the trace cache. Which patterns a cut
// line belongs to, and their spans, are decided on the whole line read
// again from the file (creditPatterns), while a cache hit reads only the
// text the bounds keep; caching only whole lines keeps the hit's answer
// the scan's. The scan is repeated instead. Whether a pattern's own
// submatches were left out is decided after crediting (submatchesLeftOut).
func linesCut(matches []MatchRaw, contexts []ContextRaw) bool {
	for _, m := range matches {
		if m.LineTextTruncated {
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

// fileOutcome is what reading one file gave the trace: the lines a scan
// matched, still waiting for their patterns, or a cache hit's matches.
type fileOutcome struct {
	fileID string
	path   string
	size   int64
	// readTime is how long the scan or the cache-hit pass took; the
	// OnFile hook reports it.
	readTime time.Duration
	// scanned is set for a scan, nil for a cache hit.
	scanned *scannedLines
	// matches are a cache hit's matches, already labeled with their
	// patterns.
	matches []rxtypes.Match
}

// scannedLines is one file's matched lines as a scan reported them,
// before their patterns are decided.
type scannedLines struct {
	source sandbox.Pinned
	lines  []MatchRaw
	// numbered holds, for each line, its match as the answer reports it,
	// numbered, with no pattern and no submatches yet.
	numbered []rxtypes.Match
	// cacheEntry is the trace-cache record the matches go into, nil when
	// the answer is not to be cached; cacheable says whether the scan
	// may be written to it (see scanToCache, chunksCutALine, linesCut).
	cacheEntry *ScannedFile
	cacheable  bool
	// text counts the bytes of the lines' text.
	text int
}

// add records one matched line and its numbered match.
func (s *scannedLines) add(rm MatchRaw, numbered rxtypes.Match) {
	s.lines = append(s.lines, rm)
	s.numbered = append(s.numbered, numbered)
	s.text += len(rm.LineText)
}

// settleEveryFiles and settleEveryBytes bound how much a trace reads
// before it settles the files read so far: decides their lines'
// patterns, builds their matches and fires their OnFile hooks. Settling
// several files at once costs one ripgrep run per pattern for all of
// them, where settling each file alone costs that much per file (5 ms
// or so per run, which doubled the time of a search of many small
// files); settling within these bounds keeps OnFile a progress report
// of a long search, never more than this far behind the scan.
const (
	settleEveryFiles = 64
	settleEveryBytes = creditBatchBytes
)

// settleWhenDue reports whether the files read since the last settle,
// holding text bytes of matched line text, are to be settled now: with
// one pattern after every file (there is nothing to decide, so nothing
// to share), otherwise once they reach settleEveryFiles files or
// settleEveryBytes of text.
func settleWhenDue(files, text, patterns int) bool {
	return patterns == 1 || files >= settleEveryFiles || text >= settleEveryBytes
}

// settleFiles decides the patterns of every scanned line of outcomes in
// one creditPatterns call, then, file by file in outcomes' order, builds
// the matches (one per line and credited pattern), fills the trace-cache
// records, and fires the OnFile hook. It returns every match, the scans
// to write to the trace cache by path, and the paths of files lost
// because their lines could not be credited (reported as skipped, with
// no match and no hook).
//
// The error ends the trace: a pattern ripgrep refuses or a canceled
// request.
func settleFiles(
	ctx context.Context,
	opts Options,
	outcomes []fileOutcome,
	patternIDs map[string]string,
	patternOrder []string,
) ([]rxtypes.Match, map[string]*ScannedFile, []rxtypes.SkippedFile, error) {
	req := creditRequest{patternIDs: patternIDs, patternOrder: patternOrder, rgExtraArgs: opts.RgExtraArgs}
	for _, o := range outcomes {
		if o.scanned != nil {
			req.files = append(req.files, creditFile{source: o.scanned.source, lines: o.scanned.lines})
		}
	}
	credits, err := creditPatterns(ctx, req)
	if err != nil {
		return nil, nil, nil, err
	}

	var matches []rxtypes.Match
	toCache := map[string]*ScannedFile{}
	var lost []rxtypes.SkippedFile
	next := 0 // index into credits, which lists the scanned files in order
	for _, o := range outcomes {
		fileMatches := o.matches
		if o.scanned != nil {
			fc := credits[next]
			next++
			if fc.err != nil {
				lost = append(lost, rxtypes.SkippedFile{Path: o.path, Reason: skipReason(fc.err)})
				continue
			}
			fileMatches = creditedMatches(o.scanned, fc.lines)
			if o.scanned.cacheable && !submatchesLeftOut(fileMatches) {
				toCache[o.path] = o.scanned.cacheEntry
			}
		}
		matches = append(matches, fileMatches...)
		fireOnFile(ctx, opts.HookFirer, o.path, o.readTime, o.size, len(fileMatches))
	}
	return matches, toCache, lost, nil
}

// submatchesLeftOut reports whether any match leaves some of its
// pattern's submatches out (more than RX_MAX_SUBMATCHES_PER_LINE on its
// line). Such a file is not written to the trace cache: an entry stores
// every span of every match, so a hit can apply any bound and answer as
// a scan under it. Each match's own flag counts, because a pattern alone
// can find more spans on a line than all the patterns together.
func submatchesLeftOut(matches []rxtypes.Match) bool {
	return slices.ContainsFunc(matches, func(m rxtypes.Match) bool { return m.SubmatchesTruncated })
}

// creditedMatches builds a scanned file's matches: one per line and
// pattern credited to it, with that pattern's submatches, in line order.
// They also go into the file's trace-cache record, when it has one.
func creditedMatches(scan *scannedLines, credits [][]patternCredit) []rxtypes.Match {
	var out []rxtypes.Match
	for i, numbered := range scan.numbered {
		for _, credit := range credits[i] {
			m := numbered
			// Fresh pointers, so no two matches share one.
			m.RelativeLineNumber = ptrInt(*numbered.RelativeLineNumber)
			text := *numbered.LineText
			m.LineText = &text
			m.Pattern = credit.patternID
			m.Submatches = credit.submatches
			m.SubmatchesTruncated = credit.submatchesTruncated
			out = append(out, m)
			if scan.cacheEntry != nil {
				scan.cacheEntry.Matches = append(scan.cacheEntry.Matches, m)
				if scan.cacheEntry.FrameIndexByOffset != nil {
					scan.cacheEntry.FrameIndexByOffset[m.Offset] = scan.lines[i].FrameIndex
				}
			}
		}
	}
	return out
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

// fireOnFile invokes the OnFile hook for a file read, with how long the
// read took and how many matches it gave.
func fireOnFile(ctx context.Context, hf HookFirer, path string, readTime time.Duration, size int64, matches int) {
	if hf == nil {
		return
	}
	elapsedMS := int(readTime / time.Millisecond)
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

// scanToCache decides, before a file is scanned, whether the scan's
// answer is to be written to the trace cache, and if so starts the
// record with the file's identity as info saw it. It returns nil when
// the answer is not to be cached: the cache is off, a result cap is
// set, the file is one the cache is not used for (UncachedPaths), or
// the file is below the size threshold for its kind.
//
// Taking the identity here, before the scan, is what keeps a growing
// log from being cached as larger than the part that was scanned.
// compressionFormat is empty for a plain file and "zstd-seekable" for a
// seekable-zstd one, whose lower threshold ShouldCache applies.
func scanToCache(opts Options, path string, info os.FileInfo, compressionFormat string) *ScannedFile {
	compressed := compressionFormat != ""
	if !opts.usesTraceCache(path) {
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
