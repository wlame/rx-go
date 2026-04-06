package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	json "github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/models"
)

// newIndexCommand creates the "index" subcommand for building and managing file indexes.
func newIndexCommand() *cobra.Command {
	var (
		outputJSON bool
		analyze    bool
		force      bool
	)

	cmd := &cobra.Command{
		Use:   "index PATHS...",
		Short: "Build file indexes for faster search",
		Long: `Create or manage file indexes with optional analysis.

Indexes enable efficient line-based access to large text files
and accelerate repeated searches via cached line-offset mappings.

Examples:
  rx index /path/to/large.log
  rx index /path/to/dir/
  rx index /path/to/file.log --force
  rx index /path/to/file.log --analyze
  rx index /path/to/file.log --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(cmd, args, indexFlags{
				outputJSON: outputJSON,
				analyze:    analyze,
				force:      force,
			})
		},
	}

	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output results as JSON")
	cmd.Flags().BoolVarP(&analyze, "analyze", "a", false, "Include analysis stats (line lengths, etc.)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force rebuild even if cached index is valid")

	return cmd
}

// indexFlags groups all index flag values.
type indexFlags struct {
	outputJSON bool
	analyze    bool
	force      bool
}

// indexResult tracks per-file index results for JSON output.
type indexResult struct {
	Path         string              `json:"path"`
	Success      bool                `json:"success"`
	Error        string              `json:"error,omitempty"`
	IndexType    string              `json:"index_type,omitempty"`
	LineCount    *int                `json:"line_count,omitempty"`
	Checkpoints  int                 `json:"checkpoints,omitempty"`
	BuildTime    *float64            `json:"build_time_seconds,omitempty"`
	Analysis     *models.IndexAnalysis `json:"analysis,omitempty"`
	Cached       bool                `json:"cached,omitempty"`
}

// runIndex builds indexes for the given paths.
func runIndex(cmd *cobra.Command, paths []string, flags indexFlags) error {
	cfg := config.Load()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	// Expand directories into individual files.
	var filePaths []string
	for _, p := range paths {
		stat, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(errW, "Error: %s: %s\n", p, err)
			continue
		}
		if stat.IsDir() {
			expanded, err := expandDirectory(p)
			if err != nil {
				fmt.Fprintf(errW, "Error scanning %s: %s\n", p, err)
				continue
			}
			filePaths = append(filePaths, expanded...)
		} else {
			filePaths = append(filePaths, p)
		}
	}

	var results []indexResult

	for _, filePath := range filePaths {
		result := indexOneFile(filePath, &cfg, flags, errW)
		results = append(results, result)
	}

	if flags.outputJSON {
		return outputIndexJSON(w, results)
	}
	outputIndexCLI(w, results)
	return nil
}

// indexOneFile builds or loads an index for a single file.
func indexOneFile(filePath string, cfg *config.Config, flags indexFlags, errW io.Writer) indexResult {
	result := indexResult{Path: filePath}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	cachePath := index.IndexCachePath(cfg.CacheDir, absPath)

	// Check for existing valid index (unless --force).
	if !flags.force {
		existing, err := index.Load(cachePath)
		if err == nil && existing != nil && index.Validate(existing, absPath) {
			result.Success = true
			result.Cached = true
			result.IndexType = string(existing.IndexType)
			result.Checkpoints = len(existing.LineIndex)
			result.BuildTime = existing.BuildTimeSeconds
			result.Analysis = existing.Analysis

			if existing.TotalLines != nil {
				result.LineCount = existing.TotalLines
			} else if lc := existing.GetLineCount(); lc != nil {
				result.LineCount = lc
			}

			fmt.Fprintf(errW, "%s: using cached index (%d checkpoints)\n", filePath, result.Checkpoints)
			return result
		}
	}

	// Build a new index.
	fmt.Fprintf(errW, "Indexing %s...\n", filePath)
	idx, err := index.BuildIndex(absPath, cfg)
	if err != nil {
		result.Error = err.Error()
		fmt.Fprintf(errW, "%s: index build failed: %s\n", filePath, err)
		return result
	}

	// Save to cache.
	if err := index.Save(cachePath, idx); err != nil {
		fmt.Fprintf(errW, "%s: warning: could not save index cache: %s\n", filePath, err)
	}

	result.Success = true
	result.IndexType = string(idx.IndexType)
	result.Checkpoints = len(idx.LineIndex)
	result.BuildTime = idx.BuildTimeSeconds

	if flags.analyze && idx.Analysis != nil {
		result.Analysis = idx.Analysis
	}

	if idx.TotalLines != nil {
		result.LineCount = idx.TotalLines
	} else if lc := idx.GetLineCount(); lc != nil {
		result.LineCount = lc
	}

	fmt.Fprintf(errW, "  Indexed: %d checkpoints", result.Checkpoints)
	if result.LineCount != nil {
		fmt.Fprintf(errW, ", %d lines", *result.LineCount)
	}
	fmt.Fprintln(errW)

	return result
}

// expandDirectory walks a directory and returns all regular file paths.
func expandDirectory(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if d.IsDir() {
			// Skip hidden directories.
			if len(d.Name()) > 1 && d.Name()[0] == '.' {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

// outputIndexJSON marshals index results as JSON.
func outputIndexJSON(w io.Writer, results []indexResult) error {
	output := struct {
		Indexed []indexResult `json:"indexed"`
		Total   int          `json:"total"`
	}{
		Indexed: results,
		Total:   len(results),
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// outputIndexCLI writes human-readable index summary.
func outputIndexCLI(w io.Writer, results []indexResult) {
	successCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		}
	}
	fmt.Fprintf(w, "Indexed %d files\n", successCount)

	for _, r := range results {
		if !r.Success {
			fmt.Fprintf(w, "  FAIL %s: %s\n", r.Path, r.Error)
			continue
		}
		status := "built"
		if r.Cached {
			status = "cached"
		}
		lineInfo := ""
		if r.LineCount != nil {
			lineInfo = fmt.Sprintf(", %d lines", *r.LineCount)
		}
		fmt.Fprintf(w, "  %s %s: %d checkpoints%s\n", status, r.Path, r.Checkpoints, lineInfo)

		if r.Analysis != nil {
			printAnalysis(w, r.Analysis)
		}
	}
}

// printAnalysis writes analysis stats to the writer.
func printAnalysis(w io.Writer, a *models.IndexAnalysis) {
	if a.LineCount != nil {
		fmt.Fprintf(w, "    Lines: %d", *a.LineCount)
		if a.EmptyLineCount != nil {
			fmt.Fprintf(w, " (%d empty)", *a.EmptyLineCount)
		}
		fmt.Fprintln(w)
	}
	if a.LineLengthMax != nil {
		fmt.Fprintf(w, "    Line length: max=%d", *a.LineLengthMax)
		if a.LineLengthAvg != nil {
			fmt.Fprintf(w, ", avg=%.1f", *a.LineLengthAvg)
		}
		if a.LineLengthMedian != nil {
			fmt.Fprintf(w, ", median=%.1f", *a.LineLengthMedian)
		}
		if a.LineLengthP95 != nil {
			fmt.Fprintf(w, ", p95=%.1f", *a.LineLengthP95)
		}
		fmt.Fprintln(w)
	}
}
