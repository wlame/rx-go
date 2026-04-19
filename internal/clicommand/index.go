package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
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
// JSON output shape (Python-compatible per Stage 9 Round 2 S3 user rule):
//
//	{
//	  "indexed": [{path, file_type, size_bytes, created_at, ...}],
//	  "skipped": ["/path/to/below-threshold.log"],
//	  "errors": [{"path":"/missing", "error":"message"}],
//	  "total_time": 1.234
//	}
//
// Go may ADD Go-specific keys (see MIGRATION.md — e.g. go_extras for
// additional telemetry) but must not break the above keys.
func NewIndexCommand(out io.Writer) *cobra.Command {
	var (
		force      bool
		showInfo   bool
		deleteFlag bool
		jsonOutput bool
		recursive  bool
		analyze    bool
		threshold  int
	)
	cmd := &cobra.Command{
		Use:   "index PATH [PATH ...]",
		Short: "Build or inspect file indexes",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(out, indexParams{
				paths:      args,
				force:      force,
				showInfo:   showInfo,
				delete:     deleteFlag,
				jsonOutput: jsonOutput,
				recursive:  recursive,
				analyze:    analyze,
				threshold:  threshold,
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force rebuild even if valid index exists")
	cmd.Flags().BoolVarP(&showInfo, "info", "i", false, "Show index info without rebuilding")
	cmd.Flags().BoolVarP(&deleteFlag, "delete", "d", false, "Delete index for the file")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Recursively process directories")
	cmd.Flags().BoolVarP(&analyze, "analyze", "a", false, "Run full analysis with anomaly detection")
	cmd.Flags().IntVar(&threshold, "threshold", 0, "Minimum file size in MB to index (0 = env default). Ignored with --analyze.")
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
	threshold  int
}

// indexBuildResult aggregates multi-path index-build outcomes into the
// Python-compatible wrapper shape. Each slice is initialized to empty
// (not nil) so JSON marshals as `[]` not `null`, matching Python's
// default_factory=list Pydantic behavior.
type indexBuildResult struct {
	Indexed   []map[string]any `json:"indexed"`
	Skipped   []string         `json:"skipped"`
	Errors    []indexErrorItem `json:"errors"`
	TotalTime float64          `json:"total_time"`
}

// indexErrorItem is the shape of each entry in the `errors` array —
// matches Python's `[{path, error}]` exactly.
type indexErrorItem struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// runIndex dispatches based on the mutually-exclusive mode flags.
// Precedence (Python parity): --delete > --info > build.
func runIndex(out io.Writer, p indexParams) error {
	// Sandbox check each path up-front.
	for _, path := range p.paths {
		if _, err := paths.ValidatePathWithinRoots(path); err != nil &&
			!errors.Is(err, paths.ErrNoSearchRootsConfigured) {
			_ = exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
			return err
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
				_, _ = fmt.Fprintf(out, "no index found for %s\n", path)
				continue
			}
			return err
		}
		if err := os.Remove(cachePath); err != nil {
			return fmt.Errorf("remove cache: %w", err)
		}
		_, _ = fmt.Fprintf(out, "deleted index for %s\n", path)
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
			_ = exitWithError(os.Stderr, ExitFileNotFound, "no index found for %s", p.paths[0])
			return errors.New("no index")
		}
		if p.jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(idx)
		}
		_, _ = fmt.Fprintf(out, "Index for: %s\n", idx.SourcePath)
		_, _ = fmt.Fprintf(out, "  file_type: %s\n", idx.FileType)
		_, _ = fmt.Fprintf(out, "  size_bytes: %d\n", idx.SourceSizeBytes)
		_, _ = fmt.Fprintf(out, "  created_at: %s\n", idx.CreatedAt)
		_, _ = fmt.Fprintf(out, "  analysis_performed: %v\n", idx.AnalysisPerformed)
		if idx.LineCount != nil {
			_, _ = fmt.Fprintf(out, "  line_count: %d\n", *idx.LineCount)
		}
		_, _ = fmt.Fprintf(out, "  index_entries: %d\n", len(idx.LineIndex))
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
			_, _ = fmt.Fprintf(out, "%s: no index exists\n", file["path"])
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: %v entries, analysis=%v\n",
			file["path"],
			file["index"].(map[string]any)["index_entries"],
			file["index"].(map[string]any)["analysis_performed"])
	}
	return nil
}

// runIndexBuild runs the real line-offset index builder and writes the
// result to the cache for each path in p.paths.
//
// Python-parity behavior (Stage 9 Round 2 S3 user decision):
//   - Below-threshold files → `skipped` list, exit 0 (NOT an error).
//   - Nonexistent files → `errors` list, exit 1 AFTER processing all.
//   - Directory expansion → collect files under each dir (recursive when --recursive).
//   - JSON output wraps everything in {indexed, skipped, errors, total_time}.
func runIndexBuild(out io.Writer, p indexParams) error {
	t0 := time.Now()
	result := indexBuildResult{
		Indexed: []map[string]any{},
		Skipped: []string{},
		Errors:  []indexErrorItem{},
	}

	// Expand paths: stat each. Directories are walked per --recursive.
	// Matches Python's _handle_info_or_delete traversal pattern reused
	// for the build flow.
	filesToIndex := []string{}
	for _, path := range p.paths {
		info, err := os.Stat(path)
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:  path,
				Error: err.Error(),
			})
			continue
		}
		if info.IsDir() {
			entries, derr := expandDirForIndex(path, p.recursive)
			if derr != nil {
				result.Errors = append(result.Errors, indexErrorItem{
					Path:  path,
					Error: derr.Error(),
				})
				continue
			}
			filesToIndex = append(filesToIndex, entries...)
			continue
		}
		filesToIndex = append(filesToIndex, path)
	}

	// Threshold resolution (MB → bytes). --analyze bypasses the threshold
	// in Python; Go matches by not gating when p.analyze is true.
	threshold := int64(config.LargeFileMB())
	if p.threshold > 0 {
		threshold = int64(p.threshold)
	}
	thresholdBytes := threshold * 1024 * 1024

	for _, path := range filesToIndex {
		info, err := os.Stat(path)
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:  path,
				Error: err.Error(),
			})
			continue
		}
		// Below-threshold skip — Python parity for R1-B8 / S3.
		if !p.analyze && info.Size() < thresholdBytes {
			result.Skipped = append(result.Skipped, path)
			continue
		}

		// R3-B2 FIX — Honor --force=false by consulting the cache first.
		// Python's rx-python/src/rx/indexer.py checks `load_index()` then
		// `needs_rebuild()` before calling the builder. The Go port was
		// rebuilding unconditionally, which made `rx index` non-idempotent
		// and wasted CPU on warm calls (see Stage 9 Round 3 bbreaking-warm
		// benchmark: Go 687 ms vs Python 438 ms before this fix).
		//
		// LoadForSource returns:
		//   (idx, nil)              → cache hit, valid
		//   (nil, nil)              → cache hit, stale → rebuild
		//   (nil, ErrIndexNotFound) → no cache → rebuild
		//   (nil, other err)        → read/parse failure → rebuild (don't fail the call)
		if !p.force {
			if idx, loadErr := index.LoadForSource(path); loadErr == nil && idx != nil {
				// Respect analysis-mode contract: a cache built without
				// analysis can't satisfy --analyze. Fall through to a
				// rebuild in that case. (Python's needs_rebuild has the
				// same escape hatch; see indexer.py::needs_rebuild.)
				if !p.analyze || idx.AnalysisPerformed {
					cachePath := index.GetCachePath(path)
					result.Indexed = append(result.Indexed, indexEntryJSON(idx, cachePath))
					continue
				}
			}
		}

		idx, err := index.Build(path, index.BuildOptions{Analyze: p.analyze})
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:  path,
				Error: err.Error(),
			})
			continue
		}
		cachePath, err := index.Save(idx)
		if err != nil {
			result.Errors = append(result.Errors, indexErrorItem{
				Path:  path,
				Error: err.Error(),
			})
			continue
		}
		result.Indexed = append(result.Indexed, indexEntryJSON(idx, cachePath))
	}

	result.TotalTime = time.Since(t0).Seconds()

	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		writeIndexBuildHuman(out, result, p.analyze)
	}

	// Exit 1 if any hard errors (matches Python `if result.errors: sys.exit(1)`).
	if len(result.Errors) > 0 {
		return errors.New("one or more files failed to index")
	}
	return nil
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

// expandDirForIndex walks a directory and returns files within it.
// When recursive is true, walks the whole subtree; otherwise only
// top-level entries are returned (matches Python's os.listdir vs os.walk
// split in _handle_info_or_delete).
func expandDirForIndex(dir string, recursive bool) ([]string, error) {
	var files []string
	if recursive {
		werr := walkFiles(dir, &files)
		return files, werr
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		files = append(files, dir+string(os.PathSeparator)+entry.Name())
	}
	return files, nil
}

// walkFiles is a tiny recursive filepath.Walk substitute used only by
// the index command's --recursive mode.
func walkFiles(dir string, out *[]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		full := dir + string(os.PathSeparator) + entry.Name()
		if entry.IsDir() {
			if werr := walkFiles(full, out); werr != nil {
				return werr
			}
			continue
		}
		*out = append(*out, full)
	}
	return nil
}

// writeIndexBuildHuman — plain-text summary for --json=false mode.
// Matches Python's _output_human_readable shape (one line per indexed
// file, summary counters).
func writeIndexBuildHuman(out io.Writer, r indexBuildResult, analyze bool) {
	if len(r.Indexed) == 0 && len(r.Errors) == 0 && len(r.Skipped) == 0 {
		_, _ = fmt.Fprintln(out, "No files indexed.")
		return
	}
	if analyze {
		_, _ = fmt.Fprintf(out, "Indexed and analyzed %d files in %.1fs\n", len(r.Indexed), r.TotalTime)
	} else {
		_, _ = fmt.Fprintf(out, "index built for %d files in %.3fs\n", len(r.Indexed), r.TotalTime)
	}
	for _, entry := range r.Indexed {
		_, _ = fmt.Fprintf(out, "  %s: %v lines, cache=%v\n",
			entry["path"], entry["line_count"], entry["index_path"])
	}
	if len(r.Skipped) > 0 {
		_, _ = fmt.Fprintf(out, "Skipped %d files (below threshold or not text)\n", len(r.Skipped))
	}
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %s\n", e.Path, e.Error)
	}
}
