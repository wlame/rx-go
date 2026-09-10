package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/hooks"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewTraceCommand builds the `rx trace` cobra command.
//
// Parity with rx-python/src/rx/cli/trace.py (trace_command):
//   - PATTERN is the first positional arg, [PATH ...] is the rest.
//   - --regexp / -e adds additional patterns (append semantics).
//   - --path / --file adds additional paths (append semantics).
//   - --max-results caps result count.
//   - --json switches to JSON output.
//   - --no-cache and --no-index disable caching.
//   - Unknown flags (-i, -w, -A N, --case-sensitive, ...) pass through
//     to ripgrep via rg_extra_args. This is click's allow_extra_args=True
//     behavior; cobra needs explicit DisableFlagParsing=false and
//     FParseErrWhitelist.UnknownFlags=true to allow it.
func NewTraceCommand(out io.Writer) *cobra.Command {
	var (
		inputPaths     []string
		regexps        []string
		maxResults     int
		showSamples    bool
		ctxLines       int
		beforeCtx      int
		afterCtx       int
		jsonOutput     bool
		noColor        bool
		colorFlag      string
		debugMode      bool
		requestID      string
		hookOnFile     string
		hookOnMatch    string
		hookOnComplete string
		noCache        bool
		noIndex        bool
		// Stage 9 Round 2 S5 + R1-B7: `rx trace <dir>` recurses by
		// default (Python parity). `--recursive` is a Python-compat
		// no-op (default already-true). `--no-recursive` flips the
		// behavior for users who want top-level-only scans.
		recursive   bool
		noRecursive bool
	)

	cmd := &cobra.Command{
		Use:   "trace [PATTERN] [PATH ...]",
		Short: "Search files and directories for regex patterns",
		Long: "Trace files and directories for regex patterns using ripgrep.\n" +
			"If PATH is not specified, searches the current directory.\n" +
			"Use '-' as PATH or pipe input to search stdin.\n" +
			"For multiple patterns, use -e/--regexp multiple times.",
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			// Mirror click's allow_extra_args: unknown flags like -i
			// become ripgrep passthroughs instead of parse errors.
			UnknownFlags: true,
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// --no-color is the older spelling and wins, so a script
			// that already passes it keeps working.
			if noColor {
				colorFlag = "never"
			}
			return runTrace(out, traceParams{
				args:        args,
				paths:       inputPaths,
				regexps:     regexps,
				maxResults:  maxResults,
				showSamples: showSamples,
				ctxLines:    ctxLines,
				beforeCtx:   beforeCtx,
				afterCtx:    afterCtx,
				// A flag that was given as 0 means "no context", which is
				// not the same as leaving it out; only Changed() can tell
				// them apart on an int flag.
				ctxSet:         cmd.Flags().Changed("context"),
				beforeSet:      cmd.Flags().Changed("before"),
				afterSet:       cmd.Flags().Changed("after"),
				jsonOutput:     jsonOutput,
				colorFlag:      colorFlag,
				debug:          debugMode,
				requestID:      requestID,
				hookOnFile:     hookOnFile,
				hookOnMatch:    hookOnMatch,
				hookOnComplete: hookOnComplete,
				noCache:        noCache,
				noIndex:        noIndex,
				// recursive flag is advisory; actual behavior comes
				// from noRecursive (Stage 9 Round 2 S5 default-recurse).
				// We silence the unused warning by passing through.
				recursive:   recursive,
				noRecursive: noRecursive,
			})
		},
	}

	cmd.Flags().StringArrayVar(&inputPaths, "path", nil, "File or directory path (repeatable)")
	cmd.Flags().StringArrayVar(&inputPaths, "file", nil, "Alias of --path")
	cmd.Flags().StringArrayVarP(&regexps, "regexp", "e", nil, "Regex pattern (repeatable)")
	cmd.Flags().StringArrayVar(&regexps, "regex", nil, "Alias of --regexp")
	cmd.Flags().IntVar(&maxResults, "max-results", 0, "Maximum number of results (0 = unlimited)")
	cmd.Flags().BoolVar(&showSamples, "samples", false, "Show context lines around matches")
	cmd.Flags().IntVar(&ctxLines, "context", 0, "Number of lines before and after (for --samples)")
	cmd.Flags().IntVarP(&beforeCtx, "before", "B", 0, "Number of lines before match (for --samples)")
	cmd.Flags().IntVarP(&afterCtx, "after", "A", 0, "Number of lines after match (for --samples)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")
	cmd.Flags().StringVar(&colorFlag, "color", "auto",
		"Colorize output: 'always', 'never', or 'auto' (color only on a terminal)")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output (alias for --color=never)")
	cmd.Flags().BoolVar(&debugMode, "debug", false, "Enable debug mode (creates .debug_* files)")
	cmd.Flags().StringVar(&requestID, "request-id", "", "Custom request ID (auto-generated if not provided)")
	cmd.Flags().StringVar(&hookOnFile, "hook-on-file", "", "URL to call when file scan completes")
	cmd.Flags().StringVar(&hookOnMatch, "hook-on-match", "", "URL to call per match. Requires --max-results.")
	cmd.Flags().StringVar(&hookOnComplete, "hook-on-complete", "", "URL to call when trace completes")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "Disable trace cache")
	cmd.Flags().BoolVar(&noIndex, "no-index", false, "Disable file indexing")
	// -r is Python's short form for --recursive; since the default is
	// already recursive, this flag exists for Python-script compat.
	// --no-recursive is the real opt-out.
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", true,
		"Recurse into subdirectories (default: true; Python-compat flag)")
	cmd.Flags().BoolVar(&noRecursive, "no-recursive", false,
		"Stop at top-level directory entries (Go-specific escape hatch)")

	return cmd
}

// traceParams bundles the resolved flag/arg state. Keeping a dedicated
// struct keeps runTrace's signature readable.
type traceParams struct {
	args []string

	paths          []string
	regexps        []string
	maxResults     int
	showSamples    bool
	ctxLines       int
	beforeCtx      int
	afterCtx       int
	jsonOutput     bool
	colorFlag      string
	debug          bool
	requestID      string
	ctxSet         bool
	beforeSet      bool
	afterSet       bool
	hookOnFile     string
	hookOnMatch    string
	hookOnComplete string
	noCache        bool
	noIndex        bool
	// Stage 9 Round 2 S5: recursive is advisory-only (default is already
	// recursive); noRecursive flips the behavior. Keeping both flags so
	// Python scripts that pass -r continue to parse cleanly.
	recursive   bool
	noRecursive bool
}

// runTrace resolves positionals → [pattern, paths...], then dispatches
// to the trace engine.
//
// Positional-arg semantics (match Python):
//  1. The first positional is always the PATTERN unless --regexp was
//     already used (in which case it's the first PATH).
//  2. All remaining positionals are PATHs.
//  3. If no PATH is given and stdin is a pipe → read from stdin (not
//     implemented in M6; we emit a helpful error).
//  4. If no PATH is given and stdin is a TTY → default to ".".
func runTrace(out io.Writer, p traceParams) error {
	patterns, filePaths, err := resolveTracePositionals(p)
	if err != nil {
		return err
	}

	// Piped input, either named as "-" or arriving with no path at all.
	if spooled, cleanup, sErr := spoolStdinFor(filePaths); sErr != nil {
		return exitWithError(os.Stderr, ExitGenericError, "%s", sErr.Error())
	} else if cleanup != nil {
		defer cleanup()
		filePaths = spooled
	}

	// Ripgrep binary lookup. Missing rg is a 1 exit with clear error.
	if _, rgErr := exec.LookPath("rg"); rgErr != nil {
		return exitWithError(os.Stderr, ExitGenericError, "ripgrep (rg) is not installed or not on PATH")
	}

	if len(patterns) == 0 {
		return exitWithError(os.Stderr, ExitUsageError, "at least one regex pattern is required")
	}

	// The flags and RX_HOOK_ON_*_URL resolve the same way they do over
	// HTTP, so RX_DISABLE_CUSTOM_HOOKS switches the flags off here too.
	hookOverrides := hooks.HookOverrides{
		OnFileURL:     hookOverride(p.hookOnFile),
		OnMatchURL:    hookOverride(p.hookOnMatch),
		OnCompleteURL: hookOverride(p.hookOnComplete),
	}
	hookConfig := hooks.EffectiveHooks(hooks.HookEnvFromEnv(), hookOverrides)

	// SECURITY: hook URLs from the command line get the same guard the
	// HTTP layer applies to hook_on_* query parameters — scheme
	// allowlist, no credentials, and no loopback / link-local /
	// private / CGNAT target.
	if hookErr := hooks.ValidateConfig(hookConfig); hookErr != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", hookErr.Error())
	}

	// A match hook without a cap is a request for one HTTP call per
	// matching line, which on a log file is millions. The HTTP layer
	// refuses the same combination.
	if hookConfig.HasMatchHook() && p.maxResults <= 0 {
		return exitWithError(os.Stderr, ExitUsageError,
			"--max-results is required when --hook-on-match is configured.\n"+
				"This prevents accidentally triggering millions of HTTP calls.")
	}

	// Validate paths against sandbox only if one is configured. The CLI
	// is typically unsandboxed (matches Python behavior); tests can
	// opt-in via paths.SetSearchRoots.
	//
	// Stage 9 Round 2 R1-B6 fix: stat each user-supplied path up front
	// and refuse to proceed when any path is missing. Python's CLI emits
	// "❌ Error: Path not found: <path>" and exits 1; we match with
	// exit-code ExitFileNotFound (= 1 per common.go convention).
	validated := make([]string, 0, len(filePaths))
	for _, f := range filePaths {
		v, vErr := paths.ValidatePathWithinRoots(f)
		if vErr != nil {
			if errors.Is(vErr, paths.ErrNoSearchRootsConfigured) {
				validated = append(validated, f)
				continue
			}
			var perr *paths.ErrPathOutsideRoots
			if errors.As(vErr, &perr) {
				return exitWithError(os.Stderr, ExitAccessDenied, "%s", perr.Error())
			}
			return exitWithError(os.Stderr, ExitAccessDenied, "%s", vErr.Error())
		}
		validated = append(validated, v)
	}

	// Existence and readability checks. The engine tolerates a file it
	// cannot open by listing it as skipped, which is right for one
	// unreadable file inside a directory being scanned. A path the user
	// named is different: silently reporting "0 matches" for a file
	// nobody could read is an answer to a question that was never
	// asked, so it fails here with the exit code the contract gives it.
	for _, f := range validated {
		info, statErr := os.Stat(f)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return exitWithError(os.Stderr, ExitFileNotFound, "path not found: %s", f)
			}
			if os.IsPermission(statErr) {
				return exitWithError(os.Stderr, ExitAccessDenied, "permission denied: %s", f)
			}
			return exitWithError(os.Stderr, ExitGenericError, "%s: %s", f, statErr.Error())
		}
		if info.IsDir() {
			continue
		}
		handle, openErr := os.Open(f)
		if openErr != nil {
			if os.IsPermission(openErr) {
				return exitWithError(os.Stderr, ExitAccessDenied, "permission denied: %s", f)
			}
			return exitWithError(os.Stderr, ExitGenericError, "%s: %s", f, openErr.Error())
		}
		_ = handle.Close()
	}

	// Fire the engine.
	engine := trace.New()
	var maxPtr *int
	if p.maxResults > 0 {
		m := p.maxResults
		maxPtr = &m
	}

	requestID := requestIDOrNew(p.requestID)

	// Declared before the dispatcher so the deferred on_complete can
	// read the response the engine is about to produce.
	var resp *rxtypes.TraceResponse

	// The dispatcher owns a worker pool and a queue, so it exists only
	// when something is actually configured; otherwise the engine keeps
	// its no-hook fast path. Close drains the queue and Wait blocks until
	// the workers have finished, which is what stops a queued webhook
	// from being lost when the process exits.
	var firer trace.HookFirer = trace.NoopHookFirer{}
	if hookConfig.HasAny() {
		dispatcher := hooks.NewDispatcher(hooks.DispatcherConfig{
			Env:              hooks.HookEnvFromEnv(),
			RequestOverrides: hookOverrides,
			RequestID:        requestID,
		})
		defer func() {
			dispatcher.Close()
			dispatcher.Wait()
		}()
		firer = dispatcher
		defer func() {
			if resp != nil {
				dispatcher.OnComplete(resp)
			}
		}()
	}

	resp, err = engine.RunWithOptions(context.Background(), validated, patterns, trace.Options{
		MaxResults:    maxPtr,
		ContextBefore: resolveBefore(p),
		ContextAfter:  resolveAfter(p),
		NoCache:       p.noCache,
		NoIndex:       p.noIndex,
		NoRecursive:   p.noRecursive,
		HookFirer:     firer,
		RequestID:     requestID,
	})
	if err != nil {
		// A pattern ripgrep cannot compile is a usage error, and rg's own
		// message ("regex parse error: ...") says more than we could.
		if errors.Is(err, trace.ErrInvalidPattern) {
			return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
		}
		return exitWithError(os.Stderr, ExitGenericError, "trace failed: %v", err)
	}

	if p.jsonOutput {
		return writeTraceJSON(out, resp)
	}
	return writeTraceHuman(out, resp, p)
}

// hookOverride turns a flag value into the pointer HookOverrides wants.
// An empty flag means "not given", so env keeps whatever it configured;
// only a URL the user actually typed overrides it.
func hookOverride(flagValue string) *string {
	if flagValue == "" {
		return nil
	}
	return &flagValue
}

// requestIDOrNew returns the user's --request-id, or a fresh UUID v7.
// Every trace response carries one, over HTTP and on the command line
// alike, so a run can be correlated with its webhook payloads.
func requestIDOrNew(given string) string {
	if given != "" {
		return given
	}
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.New().String()
}

// resolveTracePositionals turns [PATTERN, PATH...] + --regexp flags into
// (patterns, paths) slices.
func resolveTracePositionals(p traceParams) ([]string, []string, error) {
	// Copy --regexp values first (they become the base pattern list).
	patterns := append([]string{}, p.regexps...)
	explicit := append([]string{}, p.paths...)

	// If no --regexp was given, the first positional is the pattern.
	args := p.args
	if len(patterns) == 0 && len(args) > 0 {
		patterns = append(patterns, args[0])
		args = args[1:]
	}

	// All remaining positionals are paths, unioned with --path flags.
	explicit = append(explicit, args...)

	// Default to "." when no paths supplied and stdin isn't a pipe.
	if len(explicit) == 0 && !stdinIsPipe() {
		explicit = []string{"."}
	}
	return patterns, explicit, nil
}

// spoolStdinFor resolves the path list against piped input. It returns
// the paths to search and a cleanup function when stdin was spooled, or
// a nil cleanup when there was nothing to read.
//
// "-" anywhere in the list means "read stdin here"; an empty list with a
// pipe on stdin means the same. Stdin that carries nothing falls back to
// the current directory, which is what rx-python does.
func spoolStdinFor(filePaths []string) ([]string, func(), error) {
	named := false
	for _, p := range filePaths {
		if p == "-" {
			named = true
			break
		}
	}
	piped := len(filePaths) == 0 && stdinIsPipe()
	if !named && !piped {
		return filePaths, nil, nil
	}

	spooled, cleanup, err := spoolStdin()
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, len(filePaths)+1)
	for _, p := range filePaths {
		if p != "-" {
			out = append(out, p)
		}
	}
	if spooled != "" {
		out = append(out, spooled)
	}
	if len(out) == 0 && !named {
		// A pipe that carried nothing and no path either: search here,
		// the same as a bare `rx pattern`. When "-" was named the empty
		// input is the whole request, and searching the current
		// directory instead would be a surprise measured in gigabytes.
		out = []string{"."}
	}
	return out, cleanup, nil
}

// spoolStdin writes piped input to a temporary file and returns its
// path plus the cleanup that removes it.
//
// The engine addresses matches by byte offset in a file it can re-read,
// which a pipe cannot offer: chunking, the samples resolver and the
// cache all seek. Spooling buys all of that for the cost of one copy,
// and it is what rx-python does, down to the `rx_stdin_` prefix that
// shows up as the searched path in the output.
//
// Returns an empty path when stdin carried nothing, which the caller
// reads as "no input, fall back to the current directory".
func spoolStdin() (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "rx_stdin_*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("reading stdin: %w", err)
	}
	remove := func() { _ = os.Remove(tmp.Name()) }

	written, copyErr := io.Copy(tmp, os.Stdin)
	closeErr := tmp.Close()
	if copyErr != nil {
		remove()
		return "", nil, fmt.Errorf("reading stdin: %w", copyErr)
	}
	if closeErr != nil {
		remove()
		return "", nil, fmt.Errorf("reading stdin: %w", closeErr)
	}
	if written == 0 {
		remove()
		return "", func() {}, nil
	}
	return tmp.Name(), remove, nil
}

// defaultSamplesContext is the window --samples asks for when no explicit
// --before / --after / --context was given. Matches rx-python.
const defaultSamplesContext = 3

// resolveBefore / resolveAfter follow rx-python's precedence:
// --before > --context > (3 with --samples, else 0). A flag given
// explicitly wins even when its value is 0.
func resolveBefore(p traceParams) int {
	if p.beforeSet {
		return p.beforeCtx
	}
	if p.ctxSet {
		return p.ctxLines
	}
	if p.showSamples {
		return defaultSamplesContext
	}
	return 0
}

// resolveAfter mirrors resolveBefore.
func resolveAfter(p traceParams) int {
	if p.afterSet {
		return p.afterCtx
	}
	if p.ctxSet {
		return p.ctxLines
	}
	if p.showSamples {
		return defaultSamplesContext
	}
	return 0
}

// writeTraceJSON emits the TraceResponse directly. Matches `--json` flag.
func writeTraceJSON(out io.Writer, resp any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// writeTraceHuman emits the human-readable output.
//
// The layout is shared with rx-python byte for byte: a header block, the
// match list as "file:line:offset [pattern]", and — when context was
// asked for — the context section built by internal/output.
func writeTraceHuman(out io.Writer, resp *rxtypes.TraceResponse, p traceParams) error {
	colorize, err := shouldColorize(p.colorFlag, out)
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}
	before, after := resolveBefore(p), resolveAfter(p)
	_, _ = fmt.Fprint(out, output.FormatTraceCLI(resp, output.TraceFormatOptions{
		Before:      before,
		After:       after,
		ShowContext: p.showSamples || p.ctxSet || p.beforeSet || p.afterSet,
		Colorize:    colorize,
	}))
	return nil
}
