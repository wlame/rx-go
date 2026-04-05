package cli

import (
	"fmt"
	"io"
	"os"

	json "github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/compression"
)

// newCompressCommand creates the "compress" subcommand for creating seekable zstd files.
func newCompressCommand() *cobra.Command {
	var (
		frameSize  int
		level      int
		outputJSON bool
		force      bool
	)

	cmd := &cobra.Command{
		Use:   "compress PATHS...",
		Short: "Create seekable zstd compressed files",
		Long: `Create seekable zstd compressed files for optimized rx-tool access.

Seekable zstd files enable:
  - Parallel decompression for faster search
  - Random access without full decompression
  - Fast samples extraction

Output filename is {input}.zst by default.

Examples:
  rx compress input.log
  rx compress input.log --force
  rx compress file1.log file2.log --level=6
  rx compress large.log --frame-size=8388608
  rx compress input.log --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompress(cmd, args, compressFlags{
				frameSize:  frameSize,
				level:      level,
				outputJSON: outputJSON,
				force:      force,
			})
		},
	}

	cmd.Flags().IntVar(&frameSize, "frame-size", compression.DefaultFrameSize, "Target decompressed bytes per frame (default: 4MB)")
	cmd.Flags().IntVar(&level, "level", compression.DefaultCompressionLevel, "Zstd compression level 1-22 (default: 3)")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output results as JSON")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing output files")

	return cmd
}

// compressFlags groups compress flag values.
type compressFlags struct {
	frameSize  int
	level      int
	outputJSON bool
	force      bool
}

// compressResult tracks per-file compression results for JSON output.
type compressResult struct {
	Input            string  `json:"input"`
	Output           string  `json:"output,omitempty"`
	Success          bool    `json:"success"`
	Error            string  `json:"error,omitempty"`
	CompressedSize   int64   `json:"compressed_size,omitempty"`
	DecompressedSize int64   `json:"decompressed_size,omitempty"`
	FrameCount       int     `json:"frame_count,omitempty"`
	Ratio            float64 `json:"compression_ratio,omitempty"`
}

// runCompress compresses one or more files to seekable zstd format.
func runCompress(cmd *cobra.Command, paths []string, flags compressFlags) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	var results []compressResult
	var anyFailure bool

	for _, inputPath := range paths {
		result := compressOne(inputPath, flags, errW)
		results = append(results, result)
		if !result.Success {
			anyFailure = true
		}
	}

	if flags.outputJSON {
		data, err := json.MarshalIndent(map[string]interface{}{"files": results}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal JSON: %w", err)
		}
		fmt.Fprintln(w, string(data))
	}

	if anyFailure {
		return fmt.Errorf("one or more files failed to compress")
	}
	return nil
}

// compressOne compresses a single file and returns the result.
func compressOne(inputPath string, flags compressFlags, errW io.Writer) compressResult {
	result := compressResult{Input: inputPath}

	outputPath := inputPath + ".zst"

	// Check if output already exists.
	if _, err := os.Stat(outputPath); err == nil && !flags.force {
		result.Error = fmt.Sprintf("output file exists: %s (use --force to overwrite)", outputPath)
		fmt.Fprintf(errW, "%s: %s\n", inputPath, result.Error)
		return result
	}

	// Open input file.
	inputFile, err := os.Open(inputPath)
	if err != nil {
		result.Error = fmt.Sprintf("open input: %s", err)
		fmt.Fprintf(errW, "%s: %s\n", inputPath, result.Error)
		return result
	}
	defer inputFile.Close()

	inputStat, err := inputFile.Stat()
	if err != nil {
		result.Error = fmt.Sprintf("stat input: %s", err)
		fmt.Fprintf(errW, "%s: %s\n", inputPath, result.Error)
		return result
	}

	// Progress output to stderr.
	fmt.Fprintf(errW, "Compressing %s (%d bytes)...\n", inputPath, inputStat.Size())

	// Create output file.
	outputFile, err := os.Create(outputPath)
	if err != nil {
		result.Error = fmt.Sprintf("create output: %s", err)
		fmt.Fprintf(errW, "%s: %s\n", inputPath, result.Error)
		return result
	}

	opts := compression.CompressOpts{
		FrameSize:        flags.frameSize,
		CompressionLevel: flags.level,
	}

	seekTable, err := compression.CreateSeekableZstd(inputFile, outputFile, opts)
	outputFile.Close()

	if err != nil {
		os.Remove(outputPath)
		result.Error = fmt.Sprintf("compress: %s", err)
		fmt.Fprintf(errW, "%s: %s\n", inputPath, result.Error)
		return result
	}

	// Get output file size.
	outStat, err := os.Stat(outputPath)
	if err != nil {
		result.Error = fmt.Sprintf("stat output: %s", err)
		return result
	}

	result.Success = true
	result.Output = outputPath
	result.CompressedSize = outStat.Size()
	result.DecompressedSize = inputStat.Size()
	result.FrameCount = len(seekTable.Frames)
	if outStat.Size() > 0 {
		result.Ratio = float64(inputStat.Size()) / float64(outStat.Size())
	}

	fmt.Fprintf(errW, "  Created: %s (%d bytes, %d frames, %.1f:1 ratio)\n",
		outputPath, outStat.Size(), result.FrameCount, result.Ratio)

	return result
}
