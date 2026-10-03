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

	"github.com/wlame/rx-go/internal/compressfile"
	"github.com/wlame/rx-go/internal/index"
)

// NewCompressCommand builds the `rx compress` cobra command.
//
// Parity with rx-python/src/rx/cli/compress.py:
//
//	rx compress PATH [PATH ...] -o output.zst --frame-size=4M --level=3
//	rx compress PATH --force
//	rx compress PATH --json             (Python wrapper shape)
//
// JSON wrapper:
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
// Compression uses the native Go seekable-zstd encoder; no t2sz binary
// is needed.
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
			// cmd.Context() is the context main gave ExecuteContext: it
			// ends on SIGINT or SIGTERM, which stops the encoding.
			return runCompress(cmd.Context(), out, compressParams{
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
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output .zst path (default: PATH with a compression suffix replaced by .zst, else PATH.zst)")
	// --output-dir exists for parity with rx-python:
	// src/rx/cli/compress.py accepts --output-dir=DIR and writes
	// {dir}/{basename}.zst (auto-creating DIR via os.makedirs(exist_ok=True)).
	// Mutually exclusive with --output.
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Output directory (uses the default output name inside it)")
	cmd.Flags().StringVar(&frameSize, "frame-size", "4M", "Target frame size (e.g. 4M, 16MB)")
	cmd.Flags().IntVarP(&level, "level", "l", 3, "zstd compression level (1-22)")
	cmd.Flags().BoolVarP(&force, "force", "f", false,
		"Overwrite existing output; re-encode an input that is already seekable zstd")
	cmd.Flags().BoolVar(&buildIdx, "build-index", true,
		"Build line index after compression")
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
func runCompress(ctx context.Context, out io.Writer, p compressParams) error {
	// --output and --output-dir are mutually exclusive (Python parity —
	// rx-python/src/rx/cli/compress.py raises a click UsageError).
	if p.output != "" && p.outputDir != "" {
		return exitWithError(os.Stderr, ExitUsageError,
			"--output and --output-dir are mutually exclusive")
	}

	// Multi-path + --output is illegal (matches Python's check).
	if p.output != "" && len(p.paths) > 1 {
		return exitWithError(os.Stderr, ExitUsageError,
			"--output can only be used with a single input file")
	}

	// Two inputs that write one output (app.log and app.log.gz both
	// default to app.log.zst, or one input named twice) are refused
	// before anything is written, --output-dir included: with --force
	// the second would replace the first one's output without a word,
	// and without it the second would fail after the first succeeded.
	if err := refuseSharedOutputs(p); err != nil {
		return err
	}

	// Auto-create --output-dir (Python's os.makedirs(exist_ok=True)).
	// We do this up-front so the first file's compressOneFile call doesn't
	// race with the others trying to MkdirAll concurrently (future-proofing
	// for when/if multi-file compress runs in parallel).
	if p.outputDir != "" {
		if _, err := sandboxCheck(p.outputDir); err != nil {
			return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
		}
		// Mode 0750 (rwxr-x---) matches the posture used elsewhere in rx-go
		// (e.g. internal/index/store.go Save) and satisfies gosec G301.
		// Python's os.makedirs uses 0777 & ~umask, so on a typical system
		// Python produces 0755 — slightly more permissive than Go. This is
		// a deliberate hardening: the directory holds compressed data the
		// user wrote, so world-readable is not required by default.
		if err := os.MkdirAll(p.outputDir, 0o750); err != nil {
			return exitWithError(os.Stderr, ExitGenericError,
				"failed to create --output-dir: %s", err.Error())
		}
	}

	// Level validation — zstd is 1..22.
	if p.level < 1 || p.level > 22 {
		return exitWithError(os.Stderr, ExitUsageError, "--level must be 1..22, got %d", p.level)
	}

	// Frame size parse (rejects bad input once, up front).
	frameBytes, err := ParseFrameSize(p.frameSize)
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}

	workers := p.workers
	if workers < 1 {
		workers = 1
	}

	result := compressResult{Files: []map[string]any{}}
	var failureCodes []int

	for _, inputPath := range p.paths {
		// An interrupt (ctx ended) stops the run before the next file;
		// main then exits 5 whatever this function returns.
		if ctx.Err() != nil {
			break
		}
		entry, failureCode := compressOneFile(ctx, inputPath, p, frameBytes, workers)
		result.Files = append(result.Files, entry)
		if ok, _ := entry["success"].(bool); !ok {
			failureCodes = append(failureCodes, failureCode)
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

	// A run whose every failure was the sandbox refusing a path reports
	// access denied, and one whose every failure was a missing file
	// reports not found; a run that mixes failures reports the generic
	// code, because either specific code would be only half the story.
	return multiPathFailure(failureCodes, "one or more files failed to compress")
}

// compressOneFile processes a single input path and returns the
// Python-compatible JSON entry for the `files` array. Errors are
// reported per-entry (success=false + error), matching Python's
// behavior: the loop does not abort on the first failure.
// The second return value is the exit code the failure would produce
// on its own (4 for a path the sandbox refuses, 3 for a missing input),
// which the caller combines across paths; it is ExitSuccess when the
// entry succeeded.
func compressOneFile(ctx context.Context, inputPath string, p compressParams, frameBytes int64, workers int) (map[string]any, int) {
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

	if _, err := sandboxCheck(inputPath); err != nil {
		entry["error"] = err.Error()
		return entry, ExitAccessDenied
	}

	info, err := os.Stat(inputPath)
	if err != nil {
		entry["error"] = err.Error()
		return entry, exitCodeForPathError(err)
	}
	if info.IsDir() {
		entry["error"] = fmt.Sprintf("path is a directory: %s", inputPath)
		return entry, ExitGenericError
	}

	outputPath := outputPathFor(inputPath, p)
	// SECURITY: the output path is a write target, so it goes through the
	// same sandbox as the input. The validated form is what gets created.
	validatedOutput, err := sandboxCheck(outputPath)
	if err != nil {
		entry["error"] = err.Error()
		return entry, ExitAccessDenied
	}
	outputPath = validatedOutput

	// A compound archive, a seekable input without --force and an output
	// that is the input are refused before the "already exists" rule, so
	// the message names the real reason.
	if refusal := compressfile.Check(inputPath, outputPath, p.force); refusal != nil {
		entry["error"] = compressErrorMessage(refusal)
		return entry, ExitGenericError
	}

	// Lstat, not Stat: a symbolic link holds the name even when it
	// leads nowhere, and only --force replaces it. Compress applies the
	// same rule again at the moment it puts the output in place, so a
	// file created after this check is not overwritten either.
	if _, existsErr := os.Lstat(outputPath); existsErr == nil && !p.force {
		entry["error"] = fmt.Sprintf(
			"output file already exists: %s (use --force to overwrite)", outputPath)
		return entry, ExitGenericError
	}

	// Decompress and encode through the path POST /v1/compress uses: a
	// gzip, bzip2, xz or zstd input is written as its text, so the
	// output traces like the decompressed file.
	result, err := compressfile.Compress(ctx, compressfile.Options{
		InputPath:        inputPath,
		OutputPath:       outputPath,
		FrameSize:        int(frameBytes),
		Level:            p.level,
		Workers:          workers,
		ReencodeSeekable: p.force,
		Overwrite:        p.force,
	})
	if err != nil {
		entry["error"] = compressErrorMessage(err)
		return entry, ExitGenericError
	}

	entry["success"] = true
	entry["output"] = outputPath
	entry["compressed_size"] = result.CompressedSize
	entry["decompressed_size"] = result.DecompressedSize
	entry["frame_count"] = result.FrameCount
	// compression_ratio is decompressed/compressed (a value >= 1 for
	// actual compression), rounded down to 2 decimals like rx-python.
	entry["compression_ratio"] = result.Ratio()

	// Index the file just written, so `samples --lines=N` on it can
	// decompress one frame instead of walking the stream. A failure here
	// is reported rather than fatal: the compressed file is correct and
	// usable, and `rx index` can build the index later. The
	// `index_error` key is the one rx-python uses for the same case.
	if p.buildIdx {
		idx, idxErr := index.Build(outputPath, index.BuildOptions{})
		if idxErr != nil {
			entry["index_error"] = idxErr.Error()
			return entry, ExitSuccess
		}
		if _, saveErr := index.Save(idx); saveErr != nil {
			entry["index_error"] = saveErr.Error()
			return entry, ExitSuccess
		}
		entry["index"] = map[string]any{
			"line_count":  derefInt64(idx.LineCount),
			"frame_count": derefInt(idx.FrameCount),
		}
	}

	return entry, ExitSuccess
}

// outputPathFor is the output path inputPath is compressed to, before
// the sandbox check. Precedence (Python parity):
//
//	--output        → exact path given
//	--output-dir    → {dir}/{default name}
//	neither         → {sourceDir}/{default name}
//
// The default name is the input's base name with a compression suffix
// replaced by .zst (app.log.gz → app.log.zst), the rule POST
// /v1/compress uses too (compressfile.DefaultOutputName).
func outputPathFor(inputPath string, p compressParams) string {
	if p.output != "" {
		return p.output
	}
	dir := p.outputDir
	if dir == "" {
		dir = filepath.Dir(inputPath)
	}
	return filepath.Join(dir, compressfile.DefaultOutputName(inputPath))
}

// refuseSharedOutputs returns a usage error, already printed, when two
// of p.paths would be compressed to one output path, and nil when each
// has its own. Outputs are compared in their absolute form, so
// "app.log" and "./app.log.gz" are seen to share "app.log.zst"; two
// spellings that differ in a symbolic link are not, and for those the
// output name's own rule applies (without --force the second fails,
// with it the last one stays). The work is one map entry per input.
func refuseSharedOutputs(p compressParams) error {
	// writers maps each output, in absolute form, to the first input
	// that writes it.
	writers := make(map[string]string, len(p.paths))
	for _, inputPath := range p.paths {
		output := outputPathFor(inputPath, p)
		key := output
		if abs, err := filepath.Abs(output); err == nil {
			key = abs
		}
		if first, taken := writers[key]; taken {
			return exitWithError(os.Stderr, ExitUsageError,
				"%s and %s would both be compressed to %s; compress them in separate commands and name another output with --output",
				first, inputPath, output)
		}
		writers[key] = inputPath
	}
	return nil
}

// compressRefusalHints names, for a compressfile refusal, the flag that
// gets past it. The first entry whose error matches (errors.Is) wins.
var compressRefusalHints = []struct {
	err  error
	hint string
}{
	{compressfile.ErrAlreadySeekable, " (use --force to re-encode it)"},
	{compressfile.ErrOutputIsInput, " (use --output to name another file)"},
	{compressfile.ErrOutputExists, " (use --force to overwrite)"},
}

// compressErrorMessage words a compressfile error for the CLI, with the
// hint compressRefusalHints holds for it.
func compressErrorMessage(err error) string {
	for _, h := range compressRefusalHints {
		if errors.Is(err, h.err) {
			return err.Error() + h.hint
		}
	}
	return err.Error()
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
		if msg, ok := e["index_error"].(string); ok {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg)
		}
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

// derefInt is derefInt64 for the frame count, which the schema types as
// a plain int.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
