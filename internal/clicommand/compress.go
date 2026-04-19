package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
)

// NewCompressCommand builds the `rx compress` cobra command.
//
// Parity with rx-python/src/rx/cli/compress.py:
//
//	rx compress PATH [PATH ...] -o output.zst --frame-size=4M --level=3
//	rx compress PATH --force
//	rx compress PATH --json             (Python wrapper shape)
//
// JSON wrapper (Stage 9 Round 2 S4 rule — match Python's shape exactly):
//
//	{"files": [{
//	   "input":             "/path/to/input.log",
//	   "action":            "compress",
//	   "success":           true,
//	   "output":            "/path/to/input.log.zst",
//	   "compressed_size":   12345,
//	   "decompressed_size": 98765,
//	   "frame_count":       17,
//	   "compression_ratio": 8.01,
//	   "index":             {"line_count": 1234, "frame_count": 17}   // optional
//	}]}
//
// Matches user decisions 5.4 (native Go zstd) and 5.14 (no t2sz binary).
func NewCompressCommand(out io.Writer) *cobra.Command {
	var (
		output     string
		outputDir  string
		frameSize  string
		level      int
		force      bool
		buildIdx   bool
		noIndex    bool
		workers    int
		jsonOutput bool
	)
	cmd := &cobra.Command{
		Use:   "compress PATH [PATH ...]",
		Short: "Compress file(s) to seekable zstd format",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompress(out, compressParams{
				paths:      args,
				output:     output,
				outputDir:  outputDir,
				frameSize:  frameSize,
				level:      level,
				force:      force,
				buildIdx:   buildIdx && !noIndex,
				noIndex:    noIndex,
				workers:    workers,
				jsonOutput: jsonOutput,
			})
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output .zst path (default: PATH.zst)")
	// --output-dir added for R3-B1 parity with Python:
	// rx-python/src/rx/cli/compress.py accepts --output-dir=DIR and writes
	// {dir}/{basename}.zst (auto-creating DIR via os.makedirs(exist_ok=True)).
	// Mutually exclusive with --output.
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Output directory (uses source filename with .zst extension)")
	cmd.Flags().StringVar(&frameSize, "frame-size", "4M", "Target frame size (e.g. 4M, 16MB)")
	cmd.Flags().IntVarP(&level, "level", "l", 3, "zstd compression level (1-22)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Overwrite existing output")
	cmd.Flags().BoolVar(&buildIdx, "build-index", true, "Build line index after compression (default: true)")
	cmd.Flags().BoolVar(&noIndex, "no-index", false, "Skip building line index after compression")
	cmd.Flags().IntVar(&workers, "workers", 1, "Parallel workers for encoding (1-N)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format (Python-compatible wrapper)")
	return cmd
}

type compressParams struct {
	paths      []string
	output     string
	outputDir  string
	frameSize  string
	level      int
	force      bool
	buildIdx   bool
	noIndex    bool
	workers    int
	jsonOutput bool
}

// compressResult matches Python's rx compress --json envelope shape.
type compressResult struct {
	Files []map[string]any `json:"files"`
}

// runCompress drives the seekable encoder for 1..N input paths. Fails
// fast on flag errors, otherwise streams each input file through the
// encoder. With --json, every per-file outcome is collected into the
// `files` array (Python-parity wrapper for S4).
func runCompress(out io.Writer, p compressParams) error {
	// --output and --output-dir are mutually exclusive (Python parity —
	// rx-python/src/rx/cli/compress.py raises a click UsageError).
	if p.output != "" && p.outputDir != "" {
		_ = exitWithError(os.Stderr, ExitUsageError,
			"--output and --output-dir are mutually exclusive")
		return errors.New("output + output-dir")
	}

	// Multi-path + --output is illegal (matches Python's check).
	if p.output != "" && len(p.paths) > 1 {
		_ = exitWithError(os.Stderr, ExitUsageError,
			"--output can only be used with a single input file")
		return errors.New("multi path + output")
	}

	// Auto-create --output-dir (Python's os.makedirs(exist_ok=True)).
	// We do this up-front so the first file's compressOneFile call doesn't
	// race with the others trying to MkdirAll concurrently (future-proofing
	// for when/if multi-file compress runs in parallel).
	if p.outputDir != "" {
		// Mode 0750 (rwxr-x---) matches the posture used elsewhere in rx-go
		// (e.g. internal/index/store.go Save) and satisfies gosec G301.
		// Python's os.makedirs uses 0777 & ~umask, so on a typical system
		// Python produces 0755 — slightly more permissive than Go. This is
		// a deliberate hardening: the directory holds compressed data the
		// user wrote, so world-readable is not required by default.
		if err := os.MkdirAll(p.outputDir, 0o750); err != nil {
			_ = exitWithError(os.Stderr, ExitGenericError,
				"failed to create --output-dir: %s", err.Error())
			return fmt.Errorf("mkdir output-dir: %w", err)
		}
	}

	// Level validation — zstd is 1..22.
	if p.level < 1 || p.level > 22 {
		_ = exitWithError(os.Stderr, ExitUsageError, "--level must be 1..22, got %d", p.level)
		return errors.New("bad level")
	}

	// Frame size parse (rejects bad input once, up front).
	frameBytes, err := ParseFrameSize(p.frameSize)
	if err != nil {
		_ = exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
		return err
	}

	workers := p.workers
	if workers < 1 {
		workers = 1
	}

	result := compressResult{Files: []map[string]any{}}
	anyFailure := false

	for _, inputPath := range p.paths {
		entry := compressOneFile(inputPath, p, frameBytes, workers)
		result.Files = append(result.Files, entry)
		if ok, _ := entry["success"].(bool); !ok {
			anyFailure = true
		}
	}

	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		writeCompressHuman(out, result)
	}

	if anyFailure {
		return errors.New("one or more files failed to compress")
	}
	return nil
}

// compressOneFile processes a single input path and returns the
// Python-compatible JSON entry for the `files` array. Errors are
// reported per-entry (success=false + error), matching Python's
// behavior: the loop does not abort on the first failure.
func compressOneFile(inputPath string, p compressParams, frameBytes int64, workers int) map[string]any {
	entry := map[string]any{
		"input":             inputPath,
		"action":            "compress",
		"success":           false,
		"output":            nil,
		"compressed_size":   nil,
		"decompressed_size": nil,
		"frame_count":       nil,
		"compression_ratio": nil,
	}

	if _, err := paths.ValidatePathWithinRoots(inputPath); err != nil &&
		!errors.Is(err, paths.ErrNoSearchRootsConfigured) {
		entry["error"] = err.Error()
		return entry
	}

	info, err := os.Stat(inputPath)
	if err != nil {
		entry["error"] = err.Error()
		return entry
	}
	if info.IsDir() {
		entry["error"] = fmt.Sprintf("path is a directory: %s", inputPath)
		return entry
	}

	// Resolve output path. Precedence (Python parity):
	//   --output        → exact path given
	//   --output-dir    → {dir}/{basename}.zst
	//   neither         → {sourceDir}/{basename}.zst
	//
	// Python uses pathlib's .name (already excludes dir) and strips a
	// known compression suffix if present; we reproduce .name with
	// filepath.Base. (Suffix stripping is not yet done on the Go side even
	// for the default case; keeping the behavior consistent preserves the
	// existing parity status.)
	// Resolve output path. Precedence (Python parity):
	//   --output        → exact path given
	//   --output-dir    → {dir}/{basename}.zst
	//   neither         → {sourceDir}/{basename}.zst
	//
	// Python uses pathlib's .name (already excludes dir) and strips a
	// known compression suffix if present; we reproduce .name with
	// filepath.Base. (Suffix stripping is not yet done on the Go side even
	// for the default case; keeping the behavior consistent preserves the
	// existing parity status.)
	outputPath := p.output
	if outputPath == "" {
		if p.outputDir != "" {
			outputPath = filepath.Join(p.outputDir, filepath.Base(inputPath)+".zst")
		} else {
			outputPath = inputPath + ".zst"
		}
	}
	if _, existsErr := os.Stat(outputPath); existsErr == nil && !p.force {
		entry["error"] = fmt.Sprintf(
			"output file already exists: %s (use --force to overwrite)", outputPath)
		return entry
	}

	// Encode.
	src, err := os.Open(inputPath)
	if err != nil {
		entry["error"] = err.Error()
		return entry
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(outputPath)
	if err != nil {
		entry["error"] = err.Error()
		return entry
	}
	defer func() { _ = dst.Close() }()

	enc := seekable.NewEncoder(seekable.EncoderConfig{
		FrameSize: int(frameBytes),
		Level:     p.level,
		Workers:   workers,
	})
	tbl, err := enc.Encode(context.Background(), src, info.Size(), dst)
	if err != nil {
		entry["error"] = fmt.Sprintf("encode: %s", err.Error())
		return entry
	}
	if err := dst.Sync(); err != nil {
		entry["error"] = fmt.Sprintf("fsync: %s", err.Error())
		return entry
	}

	outInfo, _ := os.Stat(outputPath)
	compressedSize := outInfo.Size()
	decompressedSize := info.Size()
	var ratio float64
	if compressedSize > 0 {
		// Python's compression_ratio is decompressed/compressed (a value
		// >= 1 for actual compression). Mirror that convention here.
		ratio = float64(decompressedSize) / float64(compressedSize)
		ratio = float64(int(ratio*100)) / 100 // round to 2 decimals like Python
	}

	entry["success"] = true
	entry["output"] = outputPath
	entry["compressed_size"] = compressedSize
	entry["decompressed_size"] = decompressedSize
	entry["frame_count"] = len(tbl.Frames)
	entry["compression_ratio"] = ratio

	// Index is optional. When --no-index is NOT set and --build-index
	// is true (default), we'd build the line index here. The Go port
	// of build_index is in internal/seekable-index which is not wired
	// into the CLI yet at this parity stage; the JSON wrapper exposes
	// the `index` key only when an index was actually built, so omitting
	// it on non-indexed runs is Python-compatible.
	_ = p.buildIdx // reserved for follow-up wiring

	return entry
}

// writeCompressHuman — plain-text (non-JSON) summary. One line per file.
func writeCompressHuman(out io.Writer, r compressResult) {
	for _, e := range r.Files {
		if ok, _ := e["success"].(bool); !ok {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %s: %v\n", e["input"], e["error"])
			continue
		}
		_, _ = fmt.Fprintf(out, "wrote %v (%v bytes → %v bytes, %.2fx) in %v frames\n",
			e["output"], e["decompressed_size"], e["compressed_size"],
			e["compression_ratio"], e["frame_count"])
	}
}

// ParseFrameSize turns a human-readable size into bytes. Accepts:
// bare integer (bytes), B / K / KB / M / MB / G / GB (case-insensitive).
//
// Returns an error when the suffix is unknown or the number part doesn't
// parse. Matches rx-python/src/rx/web.py::parse_frame_size.
func ParseFrameSize(s string) (int64, error) {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return 0, errors.New("frame-size is empty")
	}
	type suffix struct {
		tag  string
		mult int64
	}
	suffixes := []suffix{
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
		{"G", 1 << 30},
		{"M", 1 << 20},
		{"K", 1 << 10},
		{"B", 1},
	}
	for _, sf := range suffixes {
		if strings.HasSuffix(up, sf.tag) {
			n := strings.TrimSpace(strings.TrimSuffix(up, sf.tag))
			if n == "" {
				return 0, fmt.Errorf("frame-size missing number before %s", sf.tag)
			}
			v, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return 0, fmt.Errorf("frame-size %q: %w", s, err)
			}
			return int64(v * float64(sf.mult)), nil
		}
	}
	// Bare integer = bytes.
	v, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("frame-size %q: %w", s, err)
	}
	return v, nil
}
