package trace

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/wlame/rx-go/internal/rgjson"
)

// Pipeline executes dd | rg pipeline for a single task
type Pipeline struct {
	ctx      context.Context
	task     Task
	patterns []string
	caseSensitive bool
}

// NewPipeline creates a new pipeline executor
func NewPipeline(ctx context.Context, task Task, patterns []string, caseSensitive bool) *Pipeline {
	return &Pipeline{
		ctx:      ctx,
		task:     task,
		patterns: patterns,
		caseSensitive: caseSensitive,
	}
}

// Run executes the pipeline and returns matches
func (p *Pipeline) Run() ([]MatchResult, error) {
	// Calculate dd parameters
	const blockSize = 1024 * 1024 // 1MB blocks

	skip := p.task.Offset / blockSize
	skipRemainder := p.task.Offset % blockSize
	count := (p.task.Length + skipRemainder + blockSize - 1) / blockSize

	// Build dd command
	ddCmd := exec.CommandContext(p.ctx, "dd",
		fmt.Sprintf("if=%s", p.task.FilePath),
		fmt.Sprintf("bs=%d", blockSize),
		fmt.Sprintf("skip=%d", skip),
		fmt.Sprintf("count=%d", count),
		"status=none",
	)

	// Build rg command
	rgArgs := []string{
		"--json",
		"--no-heading",
		"--color=never",
		"--line-number",
	}

	// Add case sensitivity flag
	if !p.caseSensitive {
		rgArgs = append(rgArgs, "--ignore-case")
	}

	// Add patterns
	for _, pattern := range p.patterns {
		rgArgs = append(rgArgs, "-e", pattern)
	}

	// Read from stdin
	rgArgs = append(rgArgs, "-")

	rgCmd := exec.CommandContext(p.ctx, "rg", rgArgs...)

	// Connect dd stdout to rg stdin
	ddStdout, err := ddCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create dd stdout pipe: %w", err)
	}
	rgCmd.Stdin = ddStdout

	// Get rg stdout
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create rg stdout pipe: %w", err)
	}

	// Start both commands
	if err := ddCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start dd: %w", err)
	}

	if err := rgCmd.Start(); err != nil {
		ddCmd.Process.Kill()
		return nil, fmt.Errorf("failed to start rg: %w", err)
	}

	// Parse rg output
	matches, err := p.parseMatches(rgStdout)
	if err != nil {
		ddCmd.Process.Kill()
		rgCmd.Process.Kill()
		return nil, fmt.Errorf("failed to parse matches: %w", err)
	}

	// Wait for commands to complete
	rgErr := rgCmd.Wait()
	ddErr := ddCmd.Wait()

	// rg returns exit code 1 when no matches found, which is not an error
	if rgErr != nil {
		if exitErr, ok := rgErr.(*exec.ExitError); ok {
			if exitErr.ExitCode() != 1 {
				return nil, fmt.Errorf("rg failed: %w", rgErr)
			}
		} else {
			return nil, fmt.Errorf("rg failed: %w", rgErr)
		}
	}

	if ddErr != nil {
		return nil, fmt.Errorf("dd failed: %w", ddErr)
	}

	return matches, nil
}

// parseMatches parses ripgrep JSON output and filters to task range
func (p *Pipeline) parseMatches(r io.Reader) ([]MatchResult, error) {
	parser := rgjson.NewParser(r)
	var matches []MatchResult

	// Calculate actual dd offset (aligned to block boundary)
	const blockSize = 1024 * 1024
	actualDdOffset := (p.task.Offset / blockSize) * blockSize

	for {
		event, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		// Only process match events
		if event.Type != rgjson.EventTypeMatch {
			continue
		}

		// Calculate absolute offset in file
		rgOffset := event.Data.AbsoluteOffset
		absoluteOffset := actualDdOffset + rgOffset

		// Filter to task range
		if absoluteOffset < p.task.Offset || absoluteOffset >= p.task.Offset+p.task.Length {
			continue
		}

		// Extract line text
		lineText := ""
		if event.Data.Lines != nil {
			lineText = event.Data.Lines.Text
		}

		// Extract line number
		lineNumber := -1
		if event.Data.LineNumber != nil {
			lineNumber = int(*event.Data.LineNumber)
		}

		matches = append(matches, MatchResult{
			Offset:     absoluteOffset,
			LineText:   lineText,
			LineNumber: lineNumber,
		})
	}

	return matches, nil
}
