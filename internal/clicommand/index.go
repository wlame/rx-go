package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/cachefill"
	"github.com/wlame/rx-go/internal/config"
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
		force      bool
		showInfo   bool
		deleteFlag bool
		jsonOutput bool
		recursive  bool
		threshold  int
	)
	cmd := &cobra.Command{
		Use:   "index PATH [PATH ...]",
		Short: "Build or inspect file indexes",
		// Args runs before RunE, so a removed flag is reported here
		// first: `rx index --analyze` with no path gets the replacement
		// too, rather than "requires at least 1 arg(s)".
		Args: func(cmd *cobra.Command, args []string) error {
			if err := refuseRemovedIndexFlags(cmd); err != nil {
				return err
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
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
				paths:      args,
				force:      force,
				showInfo:   showInfo,
				delete:     deleteFlag,
				jsonOutput: jsonOutput,
				recursive:  recursive,
				threshold:  thresholdOverride,
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force rebuild even if valid index exists")
	cmd.Flags().BoolVarP(&showInfo, "info", "i", false, "Show index info without rebuilding")
	cmd.Flags().BoolVarP(&deleteFlag, "delete", "d", false, "Delete index for the file")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Recursively process directories")
	cmd.Flags().IntVar(&threshold, "threshold", 0,
		"Minimum file size in MB to index; 0 indexes every file. "+
			"Leave unset to use RX_LARGE_FILE_MB.")
	declareRemovedIndexFlags(cmd)
	return cmd
}

// removedIndexFlag is a flag `rx index` no longer has. It is still
// declared, hidden from --help, so a script that passes it gets message
// and exit 2 instead of cobra's "unknown flag".
type removedIndexFlag struct {
	name      string
	shorthand string
	// isBool declares the flag as a boolean, which parses without a
	// value; any other flag takes any text, so every value it is given
	// reaches the message.
	isBool  bool
	message string
}

// removedIndexFlags are the analysis flags signatures replaced, in the
// order they are reported when several are given.
var removedIndexFlags = []removedIndexFlag{
	{
		name:      "analyze",
		shorthand: "a",
		isBool:    true,
		message:   "--analyze was replaced by: rx trace --signatures PATH",
	},
	{
		name: "analyze-window-lines",
		message: "--analyze-window-lines was removed with --analyze, " +
			"which was replaced by: rx trace --signatures PATH",
	},
}

// declareRemovedIndexFlags declares every removed flag as a hidden one:
// --analyze as a boolean, so `--analyze` and `-a` parse as they always
// did, and the window flag as text, so `--analyze-window-lines=N`
// reaches the message whatever N is.
func declareRemovedIndexFlags(cmd *cobra.Command) {
	for _, flag := range removedIndexFlags {
		if flag.isBool {
			cmd.Flags().BoolP(flag.name, flag.shorthand, false, flag.message)
		} else {
			cmd.Flags().StringP(flag.name, flag.shorthand, "", flag.message)
		}
		// MarkHidden fails only for a name that was not declared, and
		// the name was declared on the line above.
		_ = cmd.Flags().MarkHidden(flag.name)
	}
}

// refuseRemovedIndexFlags returns the usage error of the first removed
// flag the command line gives, whatever its value (`--analyze=false`
// too), or nil when it gives none. The error is already printed, so
// main only turns it into exit 2.
func refuseRemovedIndexFlags(cmd *cobra.Command) error {
	for _, flag := range removedIndexFlags {
		if cmd.Flags().Changed(flag.name) {
			return exitWithError(os.Stderr, ExitUsageError, "%s", flag.message)
		}
	}
	return nil
}

type indexParams struct {
	paths      []string
	force      bool
	showInfo   bool
	delete     bool
	jsonOutput bool
	recursive  bool
	// threshold is nil when --threshold was not given. A zero VALUE is
	// an explicit "index everything"; see the flag's RunE.
	threshold *int
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
		if idx.LineCount != nil {
			_, _ = fmt.Fprintf(out, "  line_count: %d\n", *idx.LineCount)
		}
		_, _ = fmt.Fprintf(out, "  index_entries: %d\n", len(idx.LineIndex))
		writeTimeIndexHuman(out, idx.TimeIndex)
		writeJournalHuman(out, idx.Journal)
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
				"version":           idx.Version,
				"file_type":         string(idx.FileType),
				"source_size_bytes": idx.SourceSizeBytes,
				"created_at":        idx.CreatedAt,
				"line_count":        idx.LineCount,
				"index_entries":     len(idx.LineIndex),
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
		_, _ = fmt.Fprintf(out, "%s: %v entries\n",
			output.Printable(fmt.Sprint(file["path"])),
			file["index"].(map[string]any)["index_entries"])
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
	result := buildIndexes(p)
	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		writeIndexBuildHuman(out, result, indexUnitFiles)
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
func buildIndexes(p indexParams) indexBuildResult {
	t0 := time.Now()
	result := indexBuildResult{
		Indexed:     []map[string]any{},
		Skipped:     []string{},
		SkipReasons: []rxtypes.SkippedFile{},
		Errors:      []indexErrorItem{},
	}

	// Expand paths: stat each. Directories are listed per --recursive
	// by cachefill.List, the walk POST /v1/index lists a directory
	// with, so `rx index --recursive DIR` and the route's folder task
	// cover the same files in the same order. What the walk passes over
	// (a refused link, a subdirectory it cannot list) is a skip with its
	// reason, recorded here, before any file's own skip.
	filesToIndex := []cachefill.Entry{}
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
		if !info.IsDir() {
			filesToIndex = append(filesToIndex, cachefill.Entry{Path: path, Named: true})
			continue
		}
		entries, derr := listDirectory(path, p.recursive)
		if derr != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:     path,
				Error:    derr.Error(),
				exitCode: exitCodeForPathError(derr),
			})
			continue
		}
		for _, entry := range entries {
			if entry.Skip != "" {
				result.skip(entry.Path, entry.Skip)
				continue
			}
			filesToIndex = append(filesToIndex, entry)
		}
	}

	// Threshold resolution (MB → bytes).
	//
	// A supplied threshold wins whatever its value, zero included:
	// POST /v1/index has always honored an explicit 0 and so has
	// rx-python's CLI, and a number that means itself on two surfaces
	// out of three cannot mean the default on the third.
	threshold := int64(config.LargeFileMB())
	if p.threshold != nil {
		threshold = int64(*p.threshold)
	}
	settings := cachefill.IndexSettings{
		ThresholdBytes: threshold * 1024 * 1024,
		Force:          p.force,
		// Whether the index cache can store an index is checked once, at
		// the first file that is about to be built, and the answer holds
		// for the whole run. sync.OnceValue wraps index.CheckStorable so
		// that the first call runs it and every later call returns the
		// same error (or nil) without probing again.
		CheckStorable: sync.OnceValue(index.CheckStorable),
	}

	// Each file is decided by cachefill.IndexFile, the step the folder
	// task of POST /v1/index takes too: a file below the threshold or
	// whose text is not text is skipped with the reason (rx-python skips
	// both, exit 0), a valid stored index is reused without --force, as
	// rx-python reuses one, and otherwise the index is built and stored.
	for _, target := range filesToIndex {
		res := cachefill.IndexFile(context.Background(), target, settings, nil)
		switch res.Outcome {
		case cachefill.Skipped:
			result.skip(res.Path, res.Reason)
		case cachefill.Current:
			result.Indexed = append(result.Indexed, indexEntryJSON(res.Index, res.IndexPath))
		case cachefill.Filled:
			result.Indexed = append(result.Indexed, indexEntryJSON(res.Index, res.IndexPath))
			result.builtIndexes = append(result.builtIndexes, res.Index)
		default:
			result.Errors = append(result.Errors, fillFailure(res))
		}
	}

	result.TotalTime = time.Since(t0).Seconds()
	return result
}

// listDirectory pins the directory path and lists it by the tree's
// rules (cachefill.List): every file below it with recursive, the files
// directly inside it without. A link to a file or directory is followed
// only where naming its target would be allowed.
func listDirectory(path string, recursive bool) ([]cachefill.Entry, error) {
	dir, err := paths.Pin(path)
	if err != nil {
		return nil, err
	}
	return cachefill.List(dir, recursive)
}

// fillFailure is the errors entry of a file whose fill failed: for a
// named file that cannot be reached, the words and the exit code every
// command gives the failure (not found, permission denied, not a regular
// file); for a failed build, store or scan, its message and exit 1.
func fillFailure(res cachefill.FileResult) indexErrorItem {
	if res.Unreadable {
		return indexErrorItem{Path: res.Path, Error: accessFailureText(res.Err), exitCode: exitCodeForPathError(res.Err)}
	}
	return indexErrorItem{Path: res.Path, Error: res.Reason, exitCode: ExitGenericError}
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
	// The journal section, or null for a file that is not a systemd
	// journal. rx-go only.
	entry["journal"] = idx.Journal

	if idx.LineCount != nil {
		entry["line_count"] = *idx.LineCount
	}
	if idx.EmptyLineCount != nil {
		entry["empty_line_count"] = *idx.EmptyLineCount
	}
	if idx.LineEnding != nil {
		entry["line_ending"] = *idx.LineEnding
	}

	// Line-length stats (absent from a seekable-zstd index). Python
	// emits these under a `line_length` sub-object; match that shape.
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

// The words the summary of an index build counts its inputs in: rx
// index counts files, rx logs index the parts of a chain.
const (
	indexUnitFiles = "files"
	indexUnitParts = "parts"
)

// writeIndexBuildHuman renders the same block rx-python prints
// (`cli/index.py`) for a build without analysis: one line per file. The
// two must stay identical for rx index (unit indexUnitFiles), except for
// the skipped files: rx-python prints only their count, this lists each
// one with its reason. unit is the word the counts are given in.
func writeIndexBuildHuman(out io.Writer, r indexBuildResult, unit string) {
	// Nothing indexed: say so, then still report why files were passed
	// over.
	if len(r.Indexed) == 0 {
		_, _ = fmt.Fprintf(out, "No %s indexed.\n", unit)
		writeSkippedHuman(out, r.SkipReasons, unit)
		for _, e := range r.Errors {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", output.Printable(e.Path), output.Printable(e.Error))
		}
		return
	}
	_, _ = fmt.Fprintf(out, "Indexed %d %s in %.1fs\n", len(r.Indexed), unit, r.TotalTime)
	for _, idx := range r.builtIndexes {
		writeIndexEntryHuman(out, idx)
	}
	writeSkippedHuman(out, r.SkipReasons, unit)
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", output.Printable(e.Path), output.Printable(e.Error))
	}
}

// writeSkippedHuman prints the count of skipped files, in unit, and,
// under it, one line per file with the reason it was skipped. Nothing
// when no file was skipped.
func writeSkippedHuman(out io.Writer, skipped []rxtypes.SkippedFile, unit string) {
	if len(skipped) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "Skipped %d %s:\n", len(skipped), unit)
	for _, item := range skipped {
		_, _ = fmt.Fprintf(out, "  %s: %s\n", output.Printable(item.Path), output.Printable(item.Reason))
	}
}

// writeIndexEntryHuman prints one file's summary: its path, its line
// count and its size.
func writeIndexEntryHuman(out io.Writer, idx *rxtypes.UnifiedFileIndex) {
	lineInfo := "unknown lines"
	if idx.LineCount != nil {
		lineInfo = fmt.Sprintf("%s lines", output.Thousands(*idx.LineCount))
	}
	_, _ = fmt.Fprintf(out, "  %s: %s, %s\n",
		output.Printable(idx.SourcePath), lineInfo, output.HumanSize(idx.SourceSizeBytes))
}

// derefInt64 reads an optional count, treating an absent value as zero
// the way Python's formatting does.
func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
