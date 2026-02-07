package trace

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/rgjson"
)

// CompressedPipeline handles searching in compressed files
// Note: Compressed files are processed sequentially (no chunking)
type CompressedPipeline struct {
	filePath      string
	patterns      []string
	caseSensitive bool
	format        compression.Format
	ctx           context.Context
}

// NewCompressedPipeline creates a new compressed file pipeline
func NewCompressedPipeline(
	ctx context.Context,
	filePath string,
	patterns []string,
	caseSensitive bool,
	format compression.Format,
) *CompressedPipeline {
	return &CompressedPipeline{
		filePath:      filePath,
		patterns:      patterns,
		caseSensitive: caseSensitive,
		format:        format,
		ctx:           ctx,
	}
}

// Run executes the search on the compressed file
func (p *CompressedPipeline) Run() ([]MatchResult, error) {
	// Create decompression command
	decompressCmd, err := p.createDecompressCommand()
	if err != nil {
		return nil, fmt.Errorf("failed to create decompress command: %w", err)
	}

	// Create ripgrep command
	rgCmd := p.createRipgrepCommand()

	// Pipe decompressor stdout -> ripgrep stdin
	decompressStdout, err := decompressCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create decompress stdout pipe: %w", err)
	}

	rgCmd.Stdin = decompressStdout
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create rg stdout pipe: %w", err)
	}

	// Start both commands
	if err := decompressCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start decompressor: %w", err)
	}

	if err := rgCmd.Start(); err != nil {
		decompressCmd.Process.Kill()
		return nil, fmt.Errorf("failed to start ripgrep: %w", err)
	}

	// Parse ripgrep output
	matches := p.parseMatches(rgStdout)

	// Wait for ripgrep to finish
	if err := rgCmd.Wait(); err != nil {
		// ripgrep returns exit code 1 if no matches, which is not an error
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() != 1 {
				return nil, fmt.Errorf("ripgrep failed: %w", err)
			}
		}
	}

	// Wait for decompressor to finish
	if err := decompressCmd.Wait(); err != nil {
		return nil, fmt.Errorf("decompressor failed: %w", err)
	}

	return matches, nil
}

// createDecompressCommand creates the decompression command
func (p *CompressedPipeline) createDecompressCommand() (*exec.Cmd, error) {
	var cmd *exec.Cmd

	switch p.format {
	case compression.FormatGzip:
		cmd = exec.CommandContext(p.ctx, "gzip", "-cd", p.filePath)
	case compression.FormatZstd:
		cmd = exec.CommandContext(p.ctx, "zstd", "-cd", p.filePath)
	case compression.FormatBzip2:
		cmd = exec.CommandContext(p.ctx, "bzip2", "-cd", p.filePath)
	case compression.FormatXz:
		cmd = exec.CommandContext(p.ctx, "xz", "-cd", p.filePath)
	case compression.FormatLz4:
		cmd = exec.CommandContext(p.ctx, "lz4", "-cd", p.filePath)
	default:
		return nil, fmt.Errorf("unsupported compression format: %s", p.format)
	}

	return cmd, nil
}

// createRipgrepCommand creates the ripgrep command
func (p *CompressedPipeline) createRipgrepCommand() *exec.Cmd {
	args := []string{
		"--json",
		"--no-heading",
		"--color=never",
	}

	// Case sensitivity
	if p.caseSensitive {
		args = append(args, "--case-sensitive")
	} else {
		args = append(args, "--ignore-case")
	}

	// Add patterns
	for _, pattern := range p.patterns {
		args = append(args, "-e", pattern)
	}

	// Read from stdin
	args = append(args, "-")

	return exec.CommandContext(p.ctx, "rg", args...)
}

// parseMatches parses ripgrep JSON output
func (p *CompressedPipeline) parseMatches(r io.Reader) []MatchResult {
	parser := rgjson.NewParser(r)
	matches := make([]MatchResult, 0)

	for {
		event, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Log error but continue
			continue
		}

		if event.Type != "match" {
			continue
		}

		// For compressed files, we only have relative line numbers
		// (no byte offsets available)
		lineNum := 0
		if event.Data.LineNumber != nil {
			lineNum = int(*event.Data.LineNumber)
		}

		matches = append(matches, MatchResult{
			Offset:         event.Data.AbsoluteOffset, // This is offset in decompressed stream
			LineText:       event.Data.Lines.Text,
			LineNumber:     lineNum,
			PatternMatched: "", // Will be filled by collector
		})
	}

	return matches
}

// SearchCompressed searches a compressed file
func SearchCompressed(
	ctx context.Context,
	filePath string,
	patterns []string,
	caseSensitive bool,
	format compression.Format,
) ([]MatchResult, error) {
	pipeline := NewCompressedPipeline(ctx, filePath, patterns, caseSensitive, format)
	return pipeline.Run()
}
