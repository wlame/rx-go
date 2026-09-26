package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewIndexCommand builds the `rx index` cobra command.
//
// Parity with rx-python/src/rx/cli/index.py:
//
//	rx index PATH                # build or re-use
//	rx index PATH [PATH...]      # multiple paths (Python nargs=-1)
//	rx index PATH --info         # show info, don't build
//	rx index PATH --delete       # remove cached index
//	rx index PATH --analyze      # full analysis (builds anomaly data)
//	rx index PATH --json         # JSON output
//
// JSON output shape (Python-compatible per ):
//
//	{
//	  "indexed": [{path, file_type, size_bytes, created_at, ...}],
//	  "skipped": ["/path/to/below-threshold.log"],
//	  "skip_reasons": [{"path":"/path/to/below-threshold.log",
//	                    "reason":"file size 17 bytes is below threshold 52428800 bytes"}],
//	  "errors": [{"path":"/missing", "error":"message"}],
//	  "total_time": 1.234
//	}
//
// Go may ADD Go-specific keys (see MIGRATION.md — e.g. go_extras for
// additional telemetry) but must not break the above keys.
func NewIndexCommand(out io.Writer) *cobra.Command {
	return newIndexCommand(out, runIndex)
}

// newIndexCommand is NewIndexCommand with the action injected.
//
// The flag table is the part tests need and the part that must not be
// copied: a test that redeclares the flags stops testing the command the
// moment one of them changes shape, which is how `--threshold` came to
// be an int in one place and a *int in the other. Passing `run` lets a
// test capture the params the real flags produced.
func newIndexCommand(out io.Writer, run func(io.Writer, indexParams) error) *cobra.Command {
	var (
		force              bool
		showInfo           bool
		deleteFlag         bool
		jsonOutput         bool
		recursive          bool
		analyze            bool
		threshold          int
		analyzeWindowLines int
	)
	cmd := &cobra.Command{
		Use:   "index PATH [PATH ...]",
		Short: "Build or inspect file indexes",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// "Unset" and "zero" are different answers: an explicit
			// --threshold=0 means "no threshold", and leaving the flag
			// off means "use RX_LARGE_FILE_MB". A plain int cannot hold
			// both, so the pointer is nil unless the user typed the flag.
			var thresholdOverride *int
			if cmd.Flags().Changed("threshold") {
				thresholdOverride = &threshold
			}
			return run(out, indexParams{
				paths:              args,
				force:              force,
				showInfo:           showInfo,
				delete:             deleteFlag,
				jsonOutput:         jsonOutput,
				recursive:          recursive,
				analyze:            analyze,
				threshold:          thresholdOverride,
				analyzeWindowLines: analyzeWindowLines,
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force rebuild even if valid index exists")
	cmd.Flags().BoolVarP(&showInfo, "info", "i", false, "Show index info without rebuilding")
	cmd.Flags().BoolVarP(&deleteFlag, "delete", "d", false, "Delete index for the file")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Recursively process directories")
	cmd.Flags().BoolVarP(&analyze, "analyze", "a", false, "Run full analysis with anomaly detection")
	cmd.Flags().IntVar(&threshold, "threshold", 0,
		"Minimum file size in MB to index; 0 indexes every file. "+
			"Leave unset to use RX_LARGE_FILE_MB. Ignored with --analyze.")
	// --analyze-window-lines: sliding-window size (in lines) handed to the
	// analyzer coordinator. 0 means "not set" — we fall through to the
	// RX_ANALYZE_WINDOW_LINES env var and then the compiled-in default
	// via analyzer.ResolveWindowLines. Only relevant with --analyze.
	cmd.Flags().IntVar(&analyzeWindowLines, "analyze-window-lines", 0,
		"Sliding-window size (lines) for analyzer detectors. 0 = default. Only used with --analyze.")
	return cmd
}

type indexParams struct {
	paths      []string
	force      bool
	showInfo   bool
	delete     bool
	jsonOutput bool
	recursive  bool
	analyze    bool
	// threshold is nil when --threshold was not given. A zero VALUE is
	// an explicit "index everything"; see the flag's RunE.
	threshold          *int
	analyzeWindowLines int
}

// indexBuildResult aggregates multi-path index-build outcomes into the
// Python-compatible wrapper shape. Each slice is initialized to empty
// (not nil) so JSON marshals as `[]` not `null`, matching Python's
// default_factory=list Pydantic behavior.
type indexBuildResult struct {
	Indexed []map[string]any `json:"indexed"`
	Skipped []string         `json:"skipped"`
	// SkipReasons says why each file of Skipped was passed over, in the
	// same order and in the shape a trace answer gives its
	// skip_reasons. Skipped stays a plain list of paths, the shape rx-python
	// emits, so a consumer that reads only it is unaffected.
	SkipReasons []rxtypes.SkippedFile `json:"skip_reasons"`
	Errors      []indexErrorItem      `json:"errors"`
	TotalTime   float64               `json:"total_time"`

	// builtIndexes keeps the typed indexes for the human renderer, which
	// needs fields the JSON view flattens. Never serialized.
	builtIndexes []*rxtypes.UnifiedFileIndex
}

// indexErrorItem is the shape of each entry in the `errors` array —
// matches Python's `[{path, error}]` exactly.
type indexErrorItem struct {
	Path  string `json:"path"`
	Error string `json:"error"`
	// exitCode is the code this failure would produce alone; the run's
	// exit code is derived from all of them by multiPathFailure.
	exitCode int
}

// skip records that path was not indexed and why. The reasons use the
// words POST /v1/index answers its 400 with for the same file.
func (r *indexBuildResult) skip(path, reason string) {
	r.Skipped = append(r.Skipped, path)
	r.SkipReasons = append(r.SkipReasons, rxtypes.SkippedFile{Path: path, Reason: reason})
}

// belowThresholdReason words the skip of a file smaller than the index
// threshold.
func belowThresholdReason(size, thresholdBytes int64) string {
	return fmt.Sprintf("file size %d bytes is below threshold %d bytes", size, thresholdBytes)
}

// runIndex dispatches based on the mutually-exclusive mode flags.
// Precedence (Python parity): --delete > --info > build.
func runIndex(out io.Writer, p indexParams) error {
	// Sandbox check each path up-front.
	for _, path := range p.paths {
		if _, err := paths.ValidatePathWithinRoots(path); err != nil &&
			!errors.Is(err, paths.ErrNoSearchRootsConfigured) {
			return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
		}
	}

	switch {
	case p.delete:
		return runIndexDelete(out, p)
	case p.showInfo:
		return runIndexInfo(out, p)
	default:
		return runIndexBuild(out, p)
	}
}

// runIndexDelete removes the cached index file for each path (if any).
func runIndexDelete(out io.Writer, p indexParams) error {
	for _, path := range p.paths {
		cachePath := index.GetCachePath(path)
		if _, err := os.Stat(cachePath); err != nil {
			if os.IsNotExist(err) {
				_, _ = fmt.Fprintf(out, "no index found for %s\n", output.Printable(path))
				continue
			}
			return err
		}
		if err := os.Remove(cachePath); err != nil {
			return fmt.Errorf("remove cache: %w", err)
		}
		_, _ = fmt.Fprintf(out, "deleted index for %s\n", output.Printable(path))
	}
	return nil
}

// runIndexInfo projects the cached UnifiedFileIndex to text or JSON.
//
// For a single path: emits the cached UnifiedFileIndex directly (preserves
// backwards compat with existing tools that expect the unwrapped object).
// For multiple paths: wraps in {files: [...]} matching Python's
// _handle_info_or_delete output shape.
func runIndexInfo(out io.Writer, p indexParams) error {
	if len(p.paths) == 1 {
		idx, err := index.LoadForSource(p.paths[0])
		if err != nil || idx == nil {
			return exitWithError(os.Stderr, ExitFileNotFound, "no index found for %s", p.paths[0])
		}
		if p.jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(idx)
		}
		_, _ = fmt.Fprintf(out, "Index for: %s\n", output.Printable(idx.SourcePath))
		_, _ = fmt.Fprintf(out, "  file_type: %s\n", idx.FileType)
		_, _ = fmt.Fprintf(out, "  size_bytes: %d\n", idx.SourceSizeBytes)
		_, _ = fmt.Fprintf(out, "  created_at: %s\n", idx.CreatedAt)
		_, _ = fmt.Fprintf(out, "  analysis_performed: %v\n", idx.AnalysisPerformed)
		if idx.LineCount != nil {
			_, _ = fmt.Fprintf(out, "  line_count: %d\n", *idx.LineCount)
		}
		_, _ = fmt.Fprintf(out, "  index_entries: %d\n", len(idx.LineIndex))
		writeTimeIndexHuman(out, idx.TimeIndex)
		return nil
	}

	// Multi-path info mode: Python wraps in {files: [...]}.
	files := make([]map[string]any, 0, len(p.paths))
	for _, path := range p.paths {
		entry := map[string]any{"path": path, "action": "info"}
		idx, err := index.LoadForSource(path)
		if err != nil || idx == nil {
			entry["index"] = nil
		} else {
			entry["index"] = map[string]any{
				"version":            idx.Version,
				"file_type":          string(idx.FileType),
				"source_size_bytes":  idx.SourceSizeBytes,
				"created_at":         idx.CreatedAt,
				"analysis_performed": idx.AnalysisPerformed,
				"line_count":         idx.LineCount,
				"index_entries":      len(idx.LineIndex),
			}
		}
		files = append(files, entry)
	}
	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"files": files})
	}
	for _, file := range files {
		if file["index"] == nil {
			_, _ = fmt.Fprintf(out, "%s: no index exists\n", output.Printable(fmt.Sprint(file["path"])))
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: %v entries, analysis=%v\n",
			output.Printable(fmt.Sprint(file["path"])),
			file["index"].(map[string]any)["index_entries"],
			file["index"].(map[string]any)["analysis_performed"])
	}
	return nil
}

// runIndexBuild runs the real line-offset index builder and writes the
// result to the cache for each path in p.paths.
//
// Python-parity behavior:
//   - Below-threshold files → `skipped` list, exit 0 (NOT an error).
//   - Nonexistent files → `errors` list, exit 1 AFTER processing all.
//   - Directory expansion → collect files under each dir (recursive when --recursive).
//   - JSON output wraps everything in {indexed, skipped, errors, total_time}.
func runIndexBuild(out io.Writer, p indexParams) error {
	// A negative window is a mistake the caller made, not a way to spell
	// "not set" — 0 already does that — so it is refused rather than
	// silently replaced by the default. POST /v1/index refuses it too.
	if p.analyzeWindowLines < 0 {
		return exitWithError(os.Stderr, ExitUsageError,
			"--analyze-window-lines must be positive, or 0 to use the default")
	}
	result := buildIndexes(p)
	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		writeIndexBuildHuman(out, result, p.analyze)
	}
	return multiPathFailure(result.failureCodes(), "one or more files failed to index")
}

// failureCodes are the exit codes of the files that failed, one each,
// in order: what multiPathFailure makes the run's exit code from.
func (r indexBuildResult) failureCodes() []int {
	codes := make([]int, len(r.Errors))
	for i, item := range r.Errors {
		codes[i] = item.exitCode
	}
	return codes
}

// buildIndexes builds, or reuses when current, the line index of every
// file p.paths names (each file of a directory, recursively with
// p.recursive), stores it, and returns what happened to each file. It
// prints nothing; runIndexBuild and `rx logs index` print the result.
// p.analyzeWindowLines must not be negative.
func buildIndexes(p indexParams) indexBuildResult {
	t0 := time.Now()
	result := indexBuildResult{
		Indexed:     []map[string]any{},
		Skipped:     []string{},
		SkipReasons: []rxtypes.SkippedFile{},
		Errors:      []indexErrorItem{},
	}

	// Expand paths: stat each. Directories are walked per --recursive.
	// Matches Python's _handle_info_or_delete traversal pattern reused
	// for the build flow.
	// named says whether the user named the file, rather than a walk
	// finding it: a named file that cannot be read fails the command,
	// a walked one is skipped with the reason, as `rx trace` does.
	type indexTarget struct {
		path  string
		named bool
	}
	filesToIndex := []indexTarget{}
	for _, path := range p.paths {
		info, err := os.Stat(path)
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:     path,
				Error:    err.Error(),
				exitCode: exitCodeForPathError(err),
			})
			continue
		}
		if info.IsDir() {
			// paths.WalkDir follows a symlink only when naming its
			// target would be allowed; a refused one is a skip with
			// the walk's reason, so no index is ever stored for it.
			entries, derr := paths.WalkDir(path, p.recursive)
			if derr != nil {
				result.Errors = append(result.Errors, indexErrorItem{
					Path:     path,
					Error:    derr.Error(),
					exitCode: exitCodeForPathError(derr),
				})
				continue
			}
			for _, entry := range entries {
				switch {
				case entry.ReadErr != nil:
					// A subdirectory the walk cannot list is skipped
					// with the reason and the rest of the tree is
					// indexed, as a trace of the tree searches it.
					result.skip(entry.Path, paths.FailureReason(entry.ReadErr))
				case entry.Refused != "":
					result.skip(entry.Path, entry.Refused)
				default:
					filesToIndex = append(filesToIndex, indexTarget{path: entry.Path})
				}
			}
			continue
		}
		filesToIndex = append(filesToIndex, indexTarget{path: path, named: true})
	}

	// unreadable records a file that cannot be pinned or opened: an
	// error for a file the user named, a skip for one a walk found.
	unreadable := func(target indexTarget, err error) {
		if !target.named {
			// Fixed wording: the error's own text names what the path
			// leads to, which a walked link's caller did not name.
			result.skip(target.path, paths.FailureReason(err))
			return
		}
		result.Errors = append(result.Errors, indexErrorItem{
			Path:     target.path,
			Error:    accessFailureText(err),
			exitCode: exitCodeForPathError(err),
		})
	}

	// Threshold resolution (MB → bytes). --analyze bypasses the threshold
	// in Python; Go matches by not gating when p.analyze is true.
	//
	// A supplied threshold wins whatever its value, zero included:
	// POST /v1/index has always honored an explicit 0 and so has
	// rx-python's CLI, and a number that means itself on two surfaces
	// out of three cannot mean the default on the third.
	threshold := int64(config.LargeFileMB())
	if p.threshold != nil {
		threshold = int64(*p.threshold)
	}
	thresholdBytes := threshold * 1024 * 1024

	// Hoisted out of the per-file loop: the window size only depends on
	// CLI flag and env precedence, not on the path. Compute once.
	windowLines := analyzer.ResolveWindowLines(p.analyzeWindowLines, 0)

	// Whether the index cache can store an index is checked once, at
	// the first file that is about to be built, and the answer holds for
	// the whole run. sync.OnceValue wraps index.CheckStorable so that
	// the first call runs it and every later call returns the same
	// error (or nil) without probing again.
	checkStorable := sync.OnceValue(index.CheckStorable)

	for _, target := range filesToIndex {
		path := target.path
		// The file is pinned (checked and tied to the file it leads to
		// now) and every look at it below goes through the pin.
		src, err := paths.Pin(path)
		if err != nil {
			unreadable(target, err)
			continue
		}
		info := src.Info()
		// A named pipe, a socket or a device is refused before anything
		// else looks at it; a walk has refused it already.
		if !info.Mode().IsRegular() {
			unreadable(target, fmt.Errorf("%w: %s", paths.ErrNotRegularFile, path))
			continue
		}
		// Below-threshold files are skipped rather than indexed, and the
		// command still exits 0; rx-python does the same.
		if !p.analyze && info.Size() < thresholdBytes {
			result.skip(path, belowThresholdReason(info.Size(), thresholdBytes))
			continue
		}
		// What the file is, by the rule every command applies
		// (filekind). A file whose text is not text, such as a .tar.gz
		// or a binary file, has no lines to index; rx-python skips a
		// binary file too.
		kind, err := filekind.OfPinnedForReading(src)
		if err != nil {
			unreadable(target, err)
			continue
		}
		if !kind.IsText() {
			result.skip(path, kind.NotText)
			continue
		}

		// Populate detectors from the global registry when --analyze is
		// on. LineDetectorSnapshot returns FRESH instances per call so
		// state cannot leak across files in a multi-file invocation.
		var detectors []analyzer.LineDetector
		if p.analyze {
			detectors = analyzer.LineDetectorSnapshot()
		}
		buildOpts := index.BuildOptions{
			Analyze:     p.analyze,
			WindowLines: windowLines,
			Detectors:   detectors,
		}

		// Without --force a valid cached index is reused when it answers
		// this request (index.SatisfiesBuild), as rx-python reuses one;
		// rebuilding on every call cost a full pass each time. A cache
		// that is missing, stale or unreadable means a rebuild, not a
		// failure.
		if !p.force {
			if idx, loadErr := index.LoadForSource(path); loadErr == nil && idx != nil &&
				index.SatisfiesBuild(idx, buildOpts) {
				result.Indexed = append(result.Indexed, indexEntryJSON(idx, index.GetCachePath(path)))
				continue
			}
		}

		// An index the cache cannot store is not built: the build would
		// read the whole file and the save would then fail. The file's
		// error names the cause.
		if storeErr := checkStorable(); storeErr != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:     path,
				Error:    "cannot store the line index: " + storeErr.Error(),
				exitCode: ExitGenericError,
			})
			continue
		}

		idx, err := index.Build(path, buildOpts)
		if errors.Is(err, compression.ErrTooLargeToDecode) {
			// A file rx refuses to decompress is skipped with the reason,
			// as trace skips it, not counted as a failure to index.
			result.skip(path, compression.TooLargeToDecodeReason)
			continue
		}
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:     path,
				Error:    err.Error(),
				exitCode: ExitGenericError,
			})
			continue
		}
		cachePath, err := index.Save(idx)
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:     path,
				Error:    err.Error(),
				exitCode: ExitGenericError,
			})
			continue
		}
		result.Indexed = append(result.Indexed, indexEntryJSON(idx, cachePath))
		result.builtIndexes = append(result.builtIndexes, idx)
	}

	result.TotalTime = time.Since(t0).Seconds()
	return result
}

// indexEntryJSON builds one `indexed` array entry matching Python's
// _index_to_json output. Key list is taken from rx-python/src/rx/cli/index.py.
func indexEntryJSON(idx *rxtypes.UnifiedFileIndex, cachePath string) map[string]any {
	entry := map[string]any{
		"path":               idx.SourcePath,
		"file_type":          string(idx.FileType),
		"size_bytes":         idx.SourceSizeBytes,
		"created_at":         idx.CreatedAt,
		"build_time_seconds": idx.BuildTimeSeconds,
		"analysis_performed": idx.AnalysisPerformed,
		// Go-extra: index_path exposes the cache file location. Python's
		// CLI does not emit this — it is a Go-only key documented in
		// MIGRATION.md.
		"index_path": cachePath,
	}
	// Line-index + entry count — always present per Python's behavior
	// (emits empty list when absent).
	entry["line_index"] = idx.LineIndex
	entry["index_entries"] = len(idx.LineIndex)
	// The time section, max_before included, or null for a file with
	// no timestamp format. rx-go only.
	entry["time_index"] = idx.TimeIndex

	if idx.LineCount != nil {
		entry["line_count"] = *idx.LineCount
	}
	if idx.EmptyLineCount != nil {
		entry["empty_line_count"] = *idx.EmptyLineCount
	}
	if idx.LineEnding != nil {
		entry["line_ending"] = *idx.LineEnding
	}

	// Line-length stats (only populated when --analyze). Python emits
	// these under a `line_length` sub-object; match that shape.
	if idx.LineLengthMax != nil {
		entry["line_length"] = map[string]any{
			"max":    *idx.LineLengthMax,
			"avg":    nilableFloat(idx.LineLengthAvg),
			"median": nilableFloat(idx.LineLengthMedian),
			"p95":    nilableFloat(idx.LineLengthP95),
			"p99":    nilableFloat(idx.LineLengthP99),
			"stddev": nilableFloat(idx.LineLengthStddev),
		}
		if idx.LineLengthMaxLineNumber != nil {
			entry["longest_line"] = map[string]any{
				"line_number": *idx.LineLengthMaxLineNumber,
				"byte_offset": nilableInt64(idx.LineLengthMaxByteOffset),
			}
		}
	}

	if idx.CompressionFormat != nil {
		entry["compression_format"] = *idx.CompressionFormat
	}
	if idx.DecompressedSizeBytes != nil {
		entry["decompressed_size_bytes"] = *idx.DecompressedSizeBytes
	}
	if idx.CompressionRatio != nil {
		entry["compression_ratio"] = *idx.CompressionRatio
	}

	// Anomaly info when analysis has run.
	if idx.AnalysisPerformed {
		anomalyCount := 0
		if idx.Anomalies != nil {
			anomalyCount = len(*idx.Anomalies)
		}
		entry["anomaly_count"] = anomalyCount
		entry["anomaly_summary"] = idx.AnomalySummary
		if idx.Anomalies != nil && len(*idx.Anomalies) > 0 {
			entry["anomalies"] = *idx.Anomalies
		}
	}

	return entry
}

// nilableFloat dereferences a *float64 or returns nil, suitable for
// placing directly into a JSON-bound map[string]any.
func nilableFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nilableInt64 — same pattern for *int64.
func nilableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// writeIndexBuildHuman — plain-text summary for --json=false mode.
// Matches Python's _output_human_readable shape (one line per indexed
// file, summary counters).
// writeIndexBuildHuman renders the same block rx-python prints
// (`cli/index.py`): one line per file, plus the analysis statistics and
// the anomaly summary when --analyze was used. The two must stay
// identical, except for the skipped files: rx-python prints only their
// count, this lists each one with its reason.
func writeIndexBuildHuman(out io.Writer, r indexBuildResult, analyze bool) {
	// Nothing indexed: say so, then still report why files were passed
	// over.
	if len(r.Indexed) == 0 {
		_, _ = fmt.Fprintln(out, "No files indexed.")
		writeSkippedHuman(out, r.SkipReasons)
		for _, e := range r.Errors {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", output.Printable(e.Path), output.Printable(e.Error))
		}
		return
	}
	if analyze {
		_, _ = fmt.Fprintf(out, "Indexed and analyzed %d files in %.1fs\n", len(r.Indexed), r.TotalTime)
	} else {
		_, _ = fmt.Fprintf(out, "Indexed %d files in %.1fs\n", len(r.Indexed), r.TotalTime)
	}
	for _, idx := range r.builtIndexes {
		writeIndexEntryHuman(out, idx)
	}
	writeSkippedHuman(out, r.SkipReasons)
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", output.Printable(e.Path), output.Printable(e.Error))
	}
}

// writeSkippedHuman prints the count of skipped files and, under it, one
// line per file with the reason it was skipped. Nothing when no file was
// skipped.
func writeSkippedHuman(out io.Writer, skipped []rxtypes.SkippedFile) {
	if len(skipped) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "Skipped %d files:\n", len(skipped))
	for _, item := range skipped {
		_, _ = fmt.Fprintf(out, "  %s: %s\n", output.Printable(item.Path), output.Printable(item.Reason))
	}
}

// writeIndexEntryHuman prints one file's summary and, when the index was
// built with analysis, its statistics and anomaly counts.
func writeIndexEntryHuman(out io.Writer, idx *rxtypes.UnifiedFileIndex) {
	lineInfo := "unknown lines"
	if idx.LineCount != nil {
		lineInfo = fmt.Sprintf("%s lines", output.Thousands(*idx.LineCount))
	}
	_, _ = fmt.Fprintf(out, "  %s: %s, %s\n",
		output.Printable(idx.SourcePath), lineInfo, output.HumanSize(idx.SourceSizeBytes))

	if !idx.AnalysisPerformed {
		return
	}
	if idx.LineCount != nil && idx.EmptyLineCount != nil {
		_, _ = fmt.Fprintf(out, "    Lines: %s total, %s empty\n",
			output.Thousands(*idx.LineCount), output.Thousands(*idx.EmptyLineCount))
	}
	if idx.LineEnding != nil {
		_, _ = fmt.Fprintf(out, "    Line ending: %s\n", *idx.LineEnding)
	}
	if idx.LineLengthMax != nil {
		_, _ = fmt.Fprintf(out,
			"    Line length: max=%d, avg=%.1f, median=%.1f, p95=%.1f, p99=%.1f, stddev=%.1f\n",
			*idx.LineLengthMax, derefFloat(idx.LineLengthAvg), derefFloat(idx.LineLengthMedian),
			derefFloat(idx.LineLengthP95), derefFloat(idx.LineLengthP99), derefFloat(idx.LineLengthStddev))
		if idx.LineLengthMaxLineNumber != nil {
			_, _ = fmt.Fprintf(out, "    Longest line: line %d, offset %d\n",
				*idx.LineLengthMaxLineNumber, derefInt64(idx.LineLengthMaxByteOffset))
		}
	}
	writeAnomalySummaryHuman(out, idx)
}

// writeAnomalySummaryHuman prints the per-category counts, and points at
// --json for the detail, which is the only place the ranges appear.
func writeAnomalySummaryHuman(out io.Writer, idx *rxtypes.UnifiedFileIndex) {
	count := 0
	if idx.Anomalies != nil {
		count = len(*idx.Anomalies)
	}
	if count == 0 {
		_, _ = fmt.Fprintln(out, "    Anomalies: none")
		return
	}
	if len(idx.AnomalySummary) > 0 {
		categories := make([]string, 0, len(idx.AnomalySummary))
		for category := range idx.AnomalySummary {
			categories = append(categories, category)
		}
		sort.Strings(categories)
		parts := make([]string, 0, len(categories))
		for _, category := range categories {
			parts = append(parts, fmt.Sprintf("%d %s", idx.AnomalySummary[category], category))
		}
		_, _ = fmt.Fprintf(out, "    Anomalies: %s\n", strings.Join(parts, ", "))
	} else {
		_, _ = fmt.Fprintf(out, "    Anomalies: %d\n", count)
	}
	_, _ = fmt.Fprintln(out, "    Use --json for the full anomaly list")
}

// derefFloat and derefInt64 read an optional statistic, treating an
// absent value as zero the way Python's formatting does.
func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
