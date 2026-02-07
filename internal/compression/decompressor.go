package compression

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Decompressor handles decompression of various formats
type Decompressor struct {
	detector *Detector
}

// NewDecompressor creates a new decompressor
func NewDecompressor() *Decompressor {
	return &Decompressor{
		detector: NewDetector(),
	}
}

// Decompress creates a decompression pipeline for a file
// Returns a reader that produces decompressed data
func (d *Decompressor) Decompress(ctx context.Context, filePath string) (io.ReadCloser, Format, error) {
	// Detect format
	format, err := d.detector.DetectFile(filePath)
	if err != nil {
		return nil, FormatUnknown, fmt.Errorf("failed to detect format: %w", err)
	}

	// If not compressed, return error (caller should handle directly)
	if !format.IsCompressed() {
		return nil, format, fmt.Errorf("file is not compressed")
	}

	// Create decompression command
	cmd, err := d.createDecompressCommand(ctx, filePath, format)
	if err != nil {
		return nil, format, err
	}

	// Get stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, format, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Start command
	if err := cmd.Start(); err != nil {
		return nil, format, fmt.Errorf("failed to start decompressor: %w", err)
	}

	// Return a wrapped reader that cleans up the command
	return &cmdReader{
		reader: stdout,
		cmd:    cmd,
	}, format, nil
}

// createDecompressCommand creates the appropriate decompression command
func (d *Decompressor) createDecompressCommand(ctx context.Context, filePath string, format Format) (*exec.Cmd, error) {
	var cmd *exec.Cmd

	switch format {
	case FormatGzip:
		// gzip -cd file.gz (decompress to stdout)
		cmd = exec.CommandContext(ctx, "gzip", "-cd", filePath)

	case FormatZstd:
		// zstd -cd file.zst
		cmd = exec.CommandContext(ctx, "zstd", "-cd", filePath)

	case FormatBzip2:
		// bzip2 -cd file.bz2
		cmd = exec.CommandContext(ctx, "bzip2", "-cd", filePath)

	case FormatXz:
		// xz -cd file.xz
		cmd = exec.CommandContext(ctx, "xz", "-cd", filePath)

	case FormatLz4:
		// lz4 -cd file.lz4
		cmd = exec.CommandContext(ctx, "lz4", "-cd", filePath)

	default:
		return nil, fmt.Errorf("unsupported compression format: %s", format)
	}

	return cmd, nil
}

// CheckDecompressor checks if a decompressor is available
func (d *Decompressor) CheckDecompressor(format Format) error {
	cmdName := format.DecompressorCommand()
	if cmdName == "" {
		return fmt.Errorf("no decompressor for format: %s", format)
	}

	// Check if command exists
	_, err := exec.LookPath(cmdName)
	if err != nil {
		return fmt.Errorf("decompressor '%s' not found in PATH", cmdName)
	}

	return nil
}

// cmdReader wraps a command's stdout and ensures cleanup
type cmdReader struct {
	reader io.ReadCloser
	cmd    *exec.Cmd
}

func (r *cmdReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *cmdReader) Close() error {
	// Close reader first
	if err := r.reader.Close(); err != nil {
		return err
	}

	// Wait for command to finish
	if err := r.cmd.Wait(); err != nil {
		return fmt.Errorf("decompressor failed: %w", err)
	}

	return nil
}
