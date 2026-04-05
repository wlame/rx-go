package trace

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/wlame/rx-go/internal/rgjson"
)

// StreamingPipeline executes dd | rg and streams matches as they're found
type StreamingPipeline struct {
	ctx           context.Context
	task          Task
	patterns      []string
	caseSensitive bool
	matchChan     chan<- MatchResult
}

// NewStreamingPipeline creates a pipeline that streams matches
func NewStreamingPipeline(ctx context.Context, task Task, patterns []string, caseSensitive bool, matchChan chan<- MatchResult) *StreamingPipeline {
	return &StreamingPipeline{
		ctx:           ctx,
		task:          task,
		patterns:      patterns,
		caseSensitive: caseSensitive,
		matchChan:     matchChan,
	}
}

// Run executes the pipeline and streams matches to the channel
func (p *StreamingPipeline) Run() error {
	// Calculate dd parameters
	const blockSize = 1024 * 1024 // 1MB blocks

	skip := p.task.Offset / blockSize
	skipRemainder := p.task.Offset % blockSize
	count := (p.task.Length + skipRemainder + blockSize - 1) / blockSize

	// Calculate actual dd offset for correction
	actualDdOffset := skip * blockSize

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

	if !p.caseSensitive {
		rgArgs = append(rgArgs, "--ignore-case")
	}

	for _, pattern := range p.patterns {
		rgArgs = append(rgArgs, "-e", pattern)
	}
	rgArgs = append(rgArgs, "-")

	rgCmd := exec.CommandContext(p.ctx, "rg", rgArgs...)

	// Connect dd stdout to rg stdin
	ddStdout, err := ddCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create dd stdout pipe: %w", err)
	}
	rgCmd.Stdin = ddStdout

	// Get rg stdout
	rgStdout, err := rgCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create rg stdout pipe: %w", err)
	}

	// Start both commands
	if err := ddCmd.Start(); err != nil {
		return fmt.Errorf("failed to start dd: %w", err)
	}

	if err := rgCmd.Start(); err != nil {
		ddCmd.Process.Kill()
		return fmt.Errorf("failed to start rg: %w", err)
	}

	// Stream matches as they arrive
	err = p.streamMatches(rgStdout, actualDdOffset)

	// Wait for commands to finish
	rgCmd.Wait()
	ddCmd.Wait()

	return err
}

// streamMatches reads ripgrep JSON output line-by-line and streams matches immediately
func (p *StreamingPipeline) streamMatches(reader io.Reader, actualDdOffset int64) error {
	scanner := bufio.NewScanner(reader)
	// Increase buffer size for long lines
	buf := make([]byte, 0, 1024*1024) // 1MB buffer
	scanner.Buffer(buf, 10*1024*1024) // Max 10MB lines

	for scanner.Scan() {
		// Check if context was cancelled
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		default:
		}

		line := scanner.Bytes()

		// Parse ripgrep JSON event
		event, err := rgjson.ParseEvent(line)
		if err != nil {
			continue // Skip malformed lines
		}

		// Only process match events
		if event.Type != rgjson.EventTypeMatch {
			continue
		}

		// Skip if data is incomplete
		if event.Data.Lines == nil || event.Data.LineNumber == nil {
			continue
		}

		// Calculate absolute offset
		rgOffset := event.Data.AbsoluteOffset
		absoluteOffset := actualDdOffset + rgOffset

		// Filter to task range
		if absoluteOffset < p.task.Offset || absoluteOffset >= p.task.Offset+p.task.Length {
			continue
		}

		// Create match result
		match := MatchResult{
			FilePath:       p.task.FilePath,
			Offset:         absoluteOffset,
			LineNumber:     int(*event.Data.LineNumber),
			LineText:       event.Data.Lines.Text,
			PatternMatched: "", // Will be enhanced for multi-pattern support
		}

		// Stream match immediately (non-blocking with context check)
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case p.matchChan <- match:
			// Match sent successfully
		}
	}

	return scanner.Err()
}
