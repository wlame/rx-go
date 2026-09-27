package clicommand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// newLogsTraceCommand builds `rx logs trace`: a search of log chains,
// directories and files, as GET /v1/logs/trace gives it. It takes every
// flag of `rx trace`.
//
// It never waits for background work and never reads a part to
// describe its chain: each chain is described from its parts' stored
// line indexes (logchain.Search), so a search capped by --max-results=
// reads what `rx trace` reads on the same files. A chain with a part
// not indexed yet is pending: its matches print `?` as their line in
// the chain, and stderr names `rx logs index`, which stores the
// indexes (hintAboutPendingChains). `rx logs show` and
// `rx logs samples` still read such a part, as they need its line count
// to answer at all.
func newLogsTraceCommand(out io.Writer) *cobra.Command {
	var f traceFlags
	cmd := &cobra.Command{
		Use:   "trace [PATTERN] [CHAIN|DIR|FILE ...]",
		Short: "Search log chains, directories and files; each chain's parts in its order",
		Long: "Search as rx trace does, with the parts of each log chain searched in the chain's order. " +
			"The files of a DIR are grouped into chains; a CHAIN is a chain's handle (its directory joined " +
			"with its name, as rx logs list gives it); any other path is a FILE, a part's own path included. " +
			"A match in a part prints as CHAIN:LINE (PART:LINE): TEXT, its line in the chain first; a match " +
			"in a file of its own as FILE:LINE: TEXT. Chains are described from their parts' stored line " +
			"indexes, never by reading a part: a chain with a part not indexed yet is pending, its LINE is ?, " +
			"and stderr names rx logs index CHAIN, which gives the chain lines. --json prints the " +
			"GET /v1/logs/trace body. Another " +
			"encoding of a part is skipped (duplicate_part). Without a path, the current directory. Exit 3 " +
			"for a path that is no directory, chain or file, 4 outside the search roots or for a FILE that " +
			"cannot be read, 7 when a part of a chain was renamed or replaced while it was read, 2 for a " +
			"pattern that does not compile and the usage errors of rx trace.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogsTrace(out, f.params(cmd, args))
		},
	}
	bindTraceFlags(cmd, &f)
	return cmd
}

// runLogsTrace runs one chain search and prints it.
func runLogsTrace(out io.Writer, p traceParams) error {
	patterns, filePaths, err := resolveTracePositionals(p)
	if err != nil {
		return err
	}
	if slices.Contains(filePaths, "-") {
		return exitWithError(os.Stderr, ExitUsageError, "rx logs trace searches files: it does not read standard input")
	}
	if len(filePaths) == 0 {
		// resolveTracePositionals leaves the list empty when standard
		// input is a pipe, which rx trace reads; this command does not.
		filePaths = []string{"."}
	}
	if _, rgErr := exec.LookPath("rg"); rgErr != nil {
		return exitWithError(os.Stderr, ExitGenericError, "ripgrep (rg) is not installed or not on PATH")
	}
	if len(patterns) == 0 {
		return exitWithError(os.Stderr, ExitUsageError, "at least one regex pattern is required")
	}
	hookConfig, err := traceHookConfig(p)
	if err != nil {
		return err
	}
	validated, err := validateTracePaths(filePaths)
	if err != nil {
		return err
	}

	requestID := requestIDOrNew(p.requestID)
	var resp *rxtypes.TraceResponse
	webhooks := startTraceHooks(hookConfig, requestID)
	// Go note: the deferred closure reads resp when it runs, after the
	// search below has set it.
	defer func() { webhooks.finish(resp) }()

	res, err := logchain.Search(context.Background(), trace.New(), logchain.SearchRequest{
		Paths:    validated,
		Patterns: patterns,
		Options:  traceOptions(p, requestID, webhooks.firer),
	})
	if err != nil {
		return logsTraceFailure(err)
	}
	resp = res.Trace
	warnAboutInvalidChains(os.Stderr, res.Answer)
	hintAboutPendingChains(os.Stderr, res.Answer)

	if p.jsonOutput {
		return writeTraceJSON(out, res.Answer)
	}
	colorize, err := shouldColorize(p.colorFlag, out)
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}
	_, _ = fmt.Fprint(out, output.FormatChainTraceCLI(res.Answer, traceFormatOptions(p, colorize)))
	return nil
}

// logsTraceFailure prints why a chain search failed and returns the
// exit code: 2 for a pattern rg cannot compile, 7 for a part that
// changed while its chain was described, and for a path that cannot be
// searched 4 outside the roots or into a hidden entry, 3 when it does
// not exist and names no chain, and the code of a file that cannot be
// opened otherwise.
func logsTraceFailure(err error) *ExitError {
	var pathErr *logchain.SearchPathError
	switch {
	case errors.Is(err, trace.ErrInvalidPattern):
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	case errors.Is(err, logchain.ErrPartChanged):
		return exitWithError(os.Stderr, ExitChainChanged, "a part of a log chain was renamed or replaced while it was read; run the search again")
	case errors.As(err, &pathErr):
		var outside *paths.ErrPathOutsideRoots
		var hidden *paths.ErrHiddenPath
		switch {
		case errors.As(pathErr.Err, &outside), errors.As(pathErr.Err, &hidden):
			return exitWithError(os.Stderr, ExitAccessDenied, "%s", pathErr.Err.Error())
		case errors.Is(pathErr.Err, fs.ErrNotExist):
			return exitWithError(os.Stderr, ExitFileNotFound, "path not found: %s", pathErr.Path)
		}
		return openFailure(pathErr.Path, pathErr.Err)
	}
	return exitWithError(os.Stderr, ExitGenericError, "trace failed: %v", err)
}

// warnAboutInvalidChains names on stderr each chain of the answer that
// is invalid, whose matches therefore have no line in the chain, and
// why: the answer says it in chains, and a person reading the terminal
// is told.
func warnAboutInvalidChains(w io.Writer, resp *rxtypes.ChainTraceResponse) {
	for _, id := range output.SortedIDs(resp.Chains) {
		ref := resp.Chains[id]
		if ref.State != rxtypes.ChainStateInvalid {
			continue
		}
		_, _ = fmt.Fprintf(w, "Warning: the log chain %s is invalid (%s): its matches have no line in the chain.\n",
			output.Printable(ref.Path), strings.Join(reasonWords(ref.Reasons), ", "))
	}
}

// hintAboutPendingChains names on stderr each pending chain that has a
// match in the answer, and the command that numbers its matches: a
// pending chain has a frozen part without a line index, which the
// search does not read to describe the chain, so its matches have no
// line in the chain (chain_line -1, `?` in the rows). The handle in the
// command is quoted for a shell and comes after `--`, which ends the
// command's options, so the line can be pasted as it is: a handle that
// starts with a dash (`-x.log`) is read as the chain, not as a flag.
//
// The work is one pass over the matches and one over the chains.
func hintAboutPendingChains(w io.Writer, resp *rxtypes.ChainTraceResponse) {
	matched := make(map[string]bool, len(resp.Chains))
	for _, m := range resp.Matches {
		if m.Chain != nil {
			matched[*m.Chain] = true
		}
	}
	for _, id := range output.SortedIDs(resp.Chains) {
		ref := resp.Chains[id]
		if ref.State != rxtypes.ChainStatePending || !matched[id] {
			continue
		}
		_, _ = fmt.Fprintf(w, "Hint: the log chain %s is pending (a part has no line index), so its matches have no "+
			"line in the chain: run rx logs index -- %s for chain line numbers.\n",
			output.Printable(ref.Path), output.Printable(output.Quote(ref.Path)))
	}
}
