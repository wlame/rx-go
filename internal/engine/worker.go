// worker.go runs ripgrep on a single file chunk via io.SectionReader piped to rg stdin.
//
// Instead of the Python dd|rg subprocess pipeline, we use Go's native io.SectionReader
// to supply exactly the right byte range to rg's stdin. This avoids spawning a dd process
// and gives us precise control over the read window.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/wlame/rx/internal/models"
	"github.com/wlame/rx/internal/rgjson"
)

// SearchChunk runs ripgrep on a specific byte range of a file and returns matches.
//
// How it works:
//  1. Creates an io.SectionReader over the chunk's byte range.
//  2. Builds an rg command with --json and all patterns as -e flags.
//  3. Pipes the SectionReader to rg's stdin.
//  4. Parses rg's JSON output via rgjson.Parser.
//  5. Adjusts each match's byte offset to be absolute (chunk.Offset + rg-reported offset).
//  6. Applies the dedup filter: only keeps matches whose absolute offset falls within
//     [chunk.Offset, chunk.Offset+chunk.Length). This prevents duplicates at chunk boundaries.
//
// The context controls rg lifetime — cancelling ctx kills the rg process.
func SearchChunk(
	ctx context.Context,
	file *os.File,
	chunk Chunk,
	patterns []string,
	rgExtraArgs []string,
	maxLineSize int,
) ([]models.Match, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	// Build the rg command: rg --json -e pat1 -e pat2 ... [extra args] -
	args := []string{"--json", "--no-heading", "--color=never"}
	for _, p := range patterns {
		args = append(args, "-e", p)
	}

	// Filter out incompatible flags that would conflict with --json output.
	for _, a := range rgExtraArgs {
		if a == "--byte-offset" || a == "--only-matching" {
			continue
		}
		args = append(args, a)
	}

	// "-" tells rg to read from stdin.
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, "rg", args...)

	slog.Debug("rg command",
		"chunk_index", chunk.Index,
		"chunk_offset", chunk.Offset,
		"chunk_length", chunk.Length,
		"patterns", len(patterns))

	// Pipe the chunk's byte range to rg's stdin via SectionReader.
	// SectionReader implements io.Reader and reads exactly chunk.Length bytes
	// starting at chunk.Offset — no dd subprocess needed.
	sectionReader := io.NewSectionReader(file, chunk.Offset, chunk.Length)
	cmd.Stdin = sectionReader

	// Capture stdout for JSON parsing. Stderr is discarded (rg writes warnings there).
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rg: %w", err)
	}

	// Parse rg JSON events from stdout.
	parser := rgjson.NewParser(stdout, maxLineSize)
	var matches []models.Match

	for {
		msg, err := parser.Next()
		if err != nil {
			// Log parse errors but keep going — partial results are better than none.
			break
		}
		if msg == nil {
			break // stream exhausted
		}

		if msg.Type != rgjson.TypeMatch || msg.Match == nil {
			continue
		}

		rm := msg.Match

		// rg's absolute_offset is relative to the data it received on stdin,
		// which starts at chunk.Offset in the original file.
		absoluteOffset := chunk.Offset + int64(rm.AbsoluteOffset)

		// Dedup filter: only keep matches whose start offset falls within THIS chunk's
		// designated byte range. Matches outside the range belong to an adjacent chunk.
		// This prevents duplicates when chunks overlap at newline boundaries.
		if absoluteOffset < chunk.Offset || absoluteOffset >= chunk.Offset+chunk.Length {
			continue
		}

		// Build the Match struct with all available metadata.
		lineText := strings.TrimRight(rm.Lines.Text, "\n\r")
		lineNum := rm.LineNumber

		m := models.Match{
			Pattern:            "", // Filled in later by pattern identification (phase 2).
			File:               "", // Filled in later by the orchestrator.
			Offset:             int(absoluteOffset),
			AbsoluteLineNumber: -1, // Resolved later by line number resolution.
			LineText:           &lineText,
		}

		if lineNum > 0 {
			m.RelativeLineNumber = &lineNum
		}

		// Convert rg submatches to our model.
		subs := make([]models.Submatch, len(rm.Submatches))
		for i, s := range rm.Submatches {
			subs[i] = models.Submatch{
				Text:  s.Match.Text,
				Start: s.Start,
				End:   s.End,
			}
		}
		m.Submatches = &subs

		matches = append(matches, m)
	}

	// Wait for rg to exit. Exit code 1 means "no matches" — that's not an error.
	// Exit code 2 means invalid regex (should have been caught by ValidatePattern).
	// Context cancellation produces a different error that we propagate.
	waitErr := cmd.Wait()
	if waitErr != nil {
		// Check if the context was cancelled — that's expected during eager termination.
		if ctx.Err() != nil {
			return matches, ctx.Err()
		}
		// rg exit code 1 = no matches, which is fine.
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return matches, nil
		}
		return matches, fmt.Errorf("rg exited: %w", waitErr)
	}

	return matches, nil
}
