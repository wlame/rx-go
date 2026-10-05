package trace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// patternCheckStderrLimit bounds how much of rg's stderr the pattern
// check keeps. rg echoes a pattern it cannot parse into its message, and
// a pattern is request input, so the copy kept is capped; a few lines of
// reason are all the error uses.
const patternCheckStderrLimit = 64 << 10

// rgPatternArgs is the argv of a ripgrep search of patternOrder's
// patterns over standard input, with the flags every search takes and
// the request's matching flags: the arguments the chunk, stream and
// frame scans give rg, less the context widths, which never change
// whether a pattern compiles.
func rgPatternArgs(patternIDs map[string]string, patternOrder []string, rgExtraArgs []string) []string {
	args := newRgArgs()
	for _, pid := range patternOrder {
		args = append(args, "-e", patternIDs[pid])
	}
	args = append(args, filterIncompatibleRgArgs(rgExtraArgs)...)
	return append(args, "-") // read from stdin
}

// validatePatterns runs ripgrep once over empty input with the search's
// patterns and flags, before any file is read, so a pattern rg cannot
// compile (or -P against a ripgrep without PCRE2) ends the trace as the
// pattern's error rather than surfacing later as a file rg failed on.
//
// rg compiles its patterns before it reads a byte, so an empty input is
// enough, and with no input the run can fail for no reason but the
// arguments. Any exit of 2 or more is therefore the request's fault:
// a known wording gets its row of patternFailures, and an unknown one
// is still reported as an invalid pattern with rg's own message.
// Exit 1 ("no match") is the expected answer for a pattern that
// compiles.
//
// No patterns, no check: rg would read the "-" as its pattern.
//
// Error flow: ctx's end is returned as ctx.Err(); rg that cannot be
// started or is killed is a plain error, which the CLI and HTTP layers
// report as an internal failure.
func validatePatterns(ctx context.Context, patternIDs map[string]string, patternOrder []string, rgExtraArgs []string) error {
	if len(patternOrder) == 0 {
		return nil
	}
	// exec.CommandContext kills rg if ctx ends while it runs (a request
	// canceled by its client, an interrupted CLI).
	rgCmd := exec.CommandContext(ctx, "rg", rgPatternArgs(patternIDs, patternOrder, rgExtraArgs)...)
	// Stdin and Stdout stay nil, which exec connects to the null device:
	// rg reads an empty input, and its one summary event goes nowhere,
	// so no pipe can fill and block the Wait inside Run.
	stderr := &cappedBuffer{limit: patternCheckStderrLimit}
	rgCmd.Stderr = stderr
	runErr := rgCmd.Run()
	if runErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() < 0 {
		// rg could not be started, or a signal ended it.
		return fmt.Errorf("check patterns with rg: %w", runErr)
	}
	if exitErr.ExitCode() == 1 {
		return nil
	}
	message := strings.TrimSpace(stderr.String())
	if failure := patternFailureOf(message); failure != nil {
		return failure.describe(message, patternIDs, patternOrder)
	}
	return invalidPatternError(message, patternIDs, patternOrder)
}

// cappedBuffer is an io.Writer that keeps the first limit bytes written
// to it and drops the rest. Every Write reports the whole of p as
// written, so the process writing into it never sees a failed write.
type cappedBuffer struct {
	limit int
	kept  strings.Builder
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.kept.Len(); room > 0 {
		b.kept.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.kept.String() }
