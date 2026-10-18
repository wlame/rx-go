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
//   - ripgrep's matching flags (-i, -w, -x, -F, -P) are accepted under
//     ripgrep's own spelling; see trace.MatchingFlags. Any other flag is
//     a usage error, exit 2.
func NewTraceCommand(out io.Writer) *cobra.Command {
	var flags traceFlags
	var signatureSelection string
	cmd := &cobra.Command{
		Use:   "trace [PATTERN] [PATH ...]",
		Short: "Search files and directories for regex patterns",
		Long: "Trace files and directories for regex patterns using ripgrep.\n" +
			"If PATH is not specified, searches the current directory.\n" +
			"Use '-' as PATH or pipe input to search stdin.\n" +
			"For multiple patterns, use -e/--regexp multiple times.\n" +
			"With --signatures, every positional is a PATH and patterns go with -e.",
		RunE: func(cmd *cobra.Command, args []string) error {
			p := flags.params(cmd, args)
			// Changed tells a given --signatures from an absent one;
			// the value alone cannot, since a bare flag reads "default".
			if cmd.Flags().Changed(signaturesFlag) {
				p.signatures = &signatureSelection
			}
			p.maskSecrets = maskSecretsMode(cmd)
			return runTrace(commandContext(cmd), out, p)
		},
	}
	bindTraceFlags(cmd, &flags)
	bindSignaturesFlag(cmd, &signatureSelection)
	bindMaskSecretsFlag(cmd)
	return cmd
}

// commandContext is the context cobra runs cmd with (canceled by an
// interrupt, see cmd/rx), or a context that is never canceled when the
// command runs without one.
func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// traceFlags holds the values of the flags `rx trace` and
// `rx logs trace` share; bindTraceFlags declares them.
type traceFlags struct {
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
	requestID      string
	hookOnFile     string
	hookOnMatch    string
	hookOnComplete string
	noCache        bool
	noIndex        bool
	// `rx trace <dir>` recurses by
	// default (Python parity). `--recursive` is a Python-compat
	// no-op (default already-true). `--no-recursive` flips the
	// behavior for users who want top-level-only scans.
	recursive   bool
	noRecursive bool
}

// bindTraceFlags declares the flags of `rx trace` on cmd, each bound to
// its field of f. `rx logs trace` declares the same ones, so a search
// of log chains takes every flag a trace takes.
func bindTraceFlags(cmd *cobra.Command, f *traceFlags) {
	cmd.Flags().StringArrayVar(&f.inputPaths, "path", nil, "File or directory path (repeatable)")
	cmd.Flags().StringArrayVar(&f.inputPaths, "file", nil, "Alias of --path")
	cmd.Flags().StringArrayVarP(&f.regexps, "regexp", "e", nil, "Regex pattern (repeatable)")
	cmd.Flags().StringArrayVar(&f.regexps, "regex", nil, "Alias of --regexp")
	cmd.Flags().IntVar(&f.maxResults, "max-results", 0, "Maximum number of results (0 = unlimited)")
	cmd.Flags().BoolVar(&f.showSamples, "samples", false, "Show context lines around matches")
	cmd.Flags().IntVar(&f.ctxLines, "context", 0, "Number of lines before and after (for --samples)")
	cmd.Flags().IntVarP(&f.beforeCtx, "before", "B", 0, "Number of lines before match (for --samples)")
	cmd.Flags().IntVarP(&f.afterCtx, "after", "A", 0, "Number of lines after match (for --samples)")
	cmd.Flags().BoolVar(&f.jsonOutput, "json", false, "Output results as JSON")
	cmd.Flags().StringVar(&f.colorFlag, "color", "auto",
		"Colorize output: 'always', 'never', or 'auto' (color only on a terminal)")
	cmd.Flags().BoolVar(&f.noColor, "no-color", false, "Disable colored output (alias for --color=never)")
	addDeprecatedDebugFlag(cmd)
	cmd.Flags().StringVar(&f.requestID, "request-id", "", "Custom request ID (auto-generated if not provided)")
	cmd.Flags().StringVar(&f.hookOnFile, "hook-on-file", "", "URL to call when file scan completes")
	cmd.Flags().StringVar(&f.hookOnMatch, "hook-on-match", "", "URL to call per match. Requires --max-results.")
	cmd.Flags().StringVar(&f.hookOnComplete, "hook-on-complete", "", "URL to call when trace completes")
	cmd.Flags().BoolVar(&f.noCache, "no-cache", false, "Disable trace cache")
	cmd.Flags().BoolVar(&f.noIndex, "no-index", false, "Disable file indexing")
	// -r is Python's short form for --recursive; since the default is
	// already recursive, this flag exists for Python-script compat.
	// --no-recursive is the real opt-out.
	cmd.Flags().BoolVarP(&f.recursive, "recursive", "r", true,
		"Recurse into subdirectories (default: true; Python-compat flag)")
	cmd.Flags().BoolVar(&f.noRecursive, "no-recursive", false,
		"Stop at top-level directory entries (Go-specific escape hatch)")
	for _, mf := range trace.MatchingFlags {
		cmd.Flags().BoolP(mf.Long, mf.Short, false, mf.Usage)
	}
}

// params turns the flags of one run, and its positional arguments,
// into traceParams.
func (f *traceFlags) params(cmd *cobra.Command, args []string) traceParams {
	// --no-color is the older spelling and wins, so a script that
	// already passes it keeps working.
	colorFlag := f.colorFlag
	if f.noColor {
		colorFlag = "never"
	}
	return traceParams{
		args:        args,
		paths:       f.inputPaths,
		regexps:     f.regexps,
		maxResults:  f.maxResults,
		showSamples: f.showSamples,
		ctxLines:    f.ctxLines,
		beforeCtx:   f.beforeCtx,
		afterCtx:    f.afterCtx,
		// A flag that was given as 0 means "no context", which is not
		// the same as leaving it out; only Changed() can tell them
		// apart on an int flag.
		ctxSet:         cmd.Flags().Changed("context"),
		beforeSet:      cmd.Flags().Changed("before"),
		afterSet:       cmd.Flags().Changed("after"),
		jsonOutput:     f.jsonOutput,
		colorFlag:      colorFlag,
		requestID:      f.requestID,
		hookOnFile:     f.hookOnFile,
		hookOnMatch:    f.hookOnMatch,
		hookOnComplete: f.hookOnComplete,
		noCache:        f.noCache,
		noIndex:        f.noIndex,
		// recursive flag is advisory; actual behavior comes from
		// noRecursive. It is passed through to keep the field in use.
		recursive:         f.recursive,
		noRecursive:       f.noRecursive,
		rgFlags:           trace.RipgrepArgs(selectedMatchingFlags(cmd)),
		matchingFlagNames: selectedMatchingFlagNames(cmd),
	}
}

// selectedMatchingFlagNames lists the long names of the matching flags
// the command line set, in the order of trace.MatchingFlags, as the
// equivalent command (cli_command) writes them.
func selectedMatchingFlagNames(cmd *cobra.Command) []string {
	selected := selectedMatchingFlags(cmd)
	names := []string{}
	for _, f := range trace.MatchingFlags {
		if selected[f.Long] {
			names = append(names, f.Long)
		}
	}
	return names
}

// selectedMatchingFlags reads back which of trace.MatchingFlags the
// command line set, keyed by long name. cobra registered each one under
// that name, so GetBool cannot fail here; a flag it does not know reads
// as unset.
func selectedMatchingFlags(cmd *cobra.Command) map[string]bool {
	selected := make(map[string]bool, len(trace.MatchingFlags))
	for _, f := range trace.MatchingFlags {
		selected[f.Long], _ = cmd.Flags().GetBool(f.Long)
	}
	return selected
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
	requestID      string
	ctxSet         bool
	beforeSet      bool
	afterSet       bool
	hookOnFile     string
	hookOnMatch    string
	hookOnComplete string
	noCache        bool
	noIndex        bool
	// recursive is advisory-only (default is already
	// recursive); noRecursive flips the behavior. Keeping both flags so
	// Python scripts that pass -r continue to parse cleanly.
	recursive   bool
	noRecursive bool
	// rgFlags are the ripgrep matching flags the user gave, already in
	// ripgrep's spelling ("-i", "-w"); matchingFlagNames are the same
	// flags by their long names ("ignore-case").
	rgFlags           []string
	matchingFlagNames []string
	// signatures is the --signatures selection, nil when the flag was
	// not given. With it, every positional is a path (see
	// resolveTracePositionals) and the files are scanned for those
	// signatures too.
	signatures *string
	// maskSecrets is the masking mode of the findings: --mask-secrets,
	// else RX_MASK_SECRETS (maskSecretsMode). It changes nothing
	// without --signatures.
	maskSecrets bool
}

// runTrace resolves positionals → [pattern, paths...], then dispatches
// to the trace engine.
//
// Positional-arg semantics (match Python):
//  1. The first positional is always the PATTERN unless --regexp was
//     already used (in which case it's the first PATH), or
//     --signatures was given (then every positional is a PATH).
//  2. All remaining positionals are PATHs.
//  3. If no PATH is given and stdin is a pipe → read from stdin, which
//     is spooled to a temporary file (see spoolStdinFor).
//  4. If no PATH is given and stdin is a TTY → default to ".".
//
// ctx is canceled by an interrupt; the search with signatures stops on
// it. The search without them runs as it always has.
func runTrace(ctx context.Context, out io.Writer, p traceParams) error {
	// The counts first, before standard input is read or ripgrep looked
	// up: a flag with no meaning is the whole answer.
	if err := checkTraceCounts(p); err != nil {
		return err
	}
	patterns, filePaths, err := resolveTracePositionals(p)
	if err != nil {
		return err
	}

	// Piped input, either named as "-" or arriving with no path at all.
	// spoolPath is the temporary file it was copied to, "" when there
	// was none.
	spoolPath := ""
	if spooled, spoolFile, cleanup, sErr := spoolStdinFor(ctx, filePaths); sErr != nil {
		if ctx.Err() != nil {
			// An interrupt while standard input was read: the process
			// exits 5 and prints nothing, as for an interrupted search.
			return ctx.Err()
		}
		return exitWithError(os.Stderr, ExitGenericError, "%s", sErr.Error())
	} else if cleanup != nil {
		defer cleanup()
		filePaths = spooled
		spoolPath = spoolFile
	}

	// Ripgrep binary lookup. Missing rg is a 1 exit with clear error. A
	// trace with signatures and no pattern runs no ripgrep.
	if len(patterns) > 0 {
		if _, rgErr := exec.LookPath("rg"); rgErr != nil {
			return exitWithError(os.Stderr, ExitGenericError, "ripgrep (rg) is not installed or not on PATH")
		}
	}

	if len(patterns) == 0 && p.signatures == nil {
		return exitWithError(os.Stderr, ExitUsageError, "at least one regex pattern is required")
	}

	// The signatures load and the selection resolves before any file is
	// checked or read, so a selection error is the whole answer (exit 2).
	var selection *signatureSelection
	if p.signatures != nil {
		if selection, err = loadSignatureSelection(*p.signatures); err != nil {
			return err
		}
	}

	hookConfig, err := traceHookConfig(p)
	if err != nil {
		return err
	}
	validated, err := validateTracePaths(filePaths)
	if err != nil {
		return err
	}

	// The spool is deleted when this command ends, so a trace cache
	// entry for it could never be read again: the engine writes none.
	// validated[i] is filePaths[i] as the engine will see it.
	var uncached []string
	for i, f := range filePaths {
		if spoolPath != "" && f == spoolPath {
			uncached = append(uncached, validated[i])
		}
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
		// Opened through a pin, as every read of a searched file is. A
		// file the process may not read fails as it does in every
		// command: "permission denied: <path>", exit code 4.
		src, pinErr := paths.Pin(f)
		if pinErr != nil {
			return openFailure(f, pinErr)
		}
		handle, openErr := src.Open()
		if openErr != nil {
			return openFailure(f, openErr)
		}
		_ = handle.Close()
	}

	// Fire the engine.
	engine := trace.New()
	requestID := requestIDOrNew(p.requestID)

	// Declared before the webhooks start so the deferred on_complete
	// can read the response the engine is about to produce.
	var resp *rxtypes.TraceResponse
	webhooks := startTraceHooks(hookConfig, requestID)
	// Go note: the deferred closure reads resp when it runs, after the
	// engine below has set it.
	defer func() { webhooks.finish(resp) }()

	opts := traceOptions(p, requestID, webhooks.firer)
	opts.UncachedPaths = uncached
	if selection != nil {
		resp, err = runSignatureTrace(ctx, engine, signatureTrace{
			paths: validated, patterns: patterns, opts: opts, params: p,
			selection: selection, spooled: uncached,
		})
	} else {
		resp, err = engine.RunWithOptions(context.Background(), validated, patterns, opts)
	}
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
	return writeTraceHuman(out, resp, p, selection.order())
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

// traceHookConfig resolves the webhooks of a trace run: the flags over
// RX_HOOK_ON_*_URL, the same way they resolve over HTTP, so
// RX_DISABLE_CUSTOM_HOOKS switches the flags off here too. A URL the
// guard refuses, or a match webhook without --max-results, is a usage
// error (exit 2).
func traceHookConfig(p traceParams) (hooks.HookConfig, error) {
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
		return hookConfig, exitWithError(os.Stderr, ExitUsageError, "%s", hookErr.Error())
	}

	// A match hook without a cap is a request for one HTTP call per
	// matching line, which on a log file is millions. The HTTP layer
	// refuses the same combination.
	if hookConfig.HasMatchHook() && p.maxResults <= 0 {
		return hookConfig, exitWithError(os.Stderr, ExitUsageError,
			"--max-results is required when --hook-on-match is configured.\n"+
				"This prevents accidentally triggering millions of HTTP calls.")
	}
	return hookConfig, nil
}

// validateTracePaths checks every path against the search roots, when
// some are configured, and returns them as the engine is to see them.
// The CLI is typically unsandboxed (matches Python behavior), and then
// each path is kept as given; tests can opt in via paths.SetSearchRoots.
// A path outside the roots, or into a hidden entry, is exit 4.
func validateTracePaths(filePaths []string) ([]string, error) {
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
				return nil, exitWithError(os.Stderr, ExitAccessDenied, "%s", perr.Error())
			}
			return nil, exitWithError(os.Stderr, ExitAccessDenied, "%s", vErr.Error())
		}
		validated = append(validated, v)
	}
	return validated, nil
}

// traceOptions are the engine's options for a trace run on the command
// line, with the run's request ID and webhook firer.
func traceOptions(p traceParams, requestID string, firer trace.HookFirer) trace.Options {
	var maxPtr *int
	if p.maxResults > 0 {
		m := p.maxResults
		maxPtr = &m
	}
	return trace.Options{
		MaxResults:    maxPtr,
		RgExtraArgs:   p.rgFlags,
		ContextBefore: resolveBefore(p),
		ContextAfter:  resolveAfter(p),
		NoCache:       p.noCache,
		NoIndex:       p.noIndex,
		NoRecursive:   p.noRecursive,
		HookFirer:     firer,
		RequestID:     requestID,
	}
}

// traceWebhooks are the webhooks of one trace run on the command line:
// the firer the engine calls, and what finish needs to send the last
// event and wait for every one queued.
type traceWebhooks struct {
	firer      trace.HookFirer
	request    *hooks.RequestHooks
	dispatcher *hooks.Dispatcher
}

// startTraceHooks starts the webhooks of a run. The dispatcher owns a
// worker pool and a queue, so it exists only when something is actually
// configured; otherwise the engine keeps its no-hook fast path.
func startTraceHooks(hookConfig hooks.HookConfig, requestID string) *traceWebhooks {
	if !hookConfig.HasAny() {
		return &traceWebhooks{firer: trace.NoopHookFirer{}}
	}
	dispatcher := hooks.NewDispatcher(hooks.DispatcherConfig{})
	// The same per-request view the HTTP handler uses: the URLs
	// resolved above, and this run's request_id in every payload.
	request := dispatcher.ForRequest(hookConfig, requestID)
	return &traceWebhooks{firer: request, request: request, dispatcher: dispatcher}
}

// finish fires trace_complete for resp, when the run produced one, then
// closes the dispatcher and waits until its workers have sent every
// queued event: that is what stops a queued webhook from being lost
// when the process exits.
func (w *traceWebhooks) finish(resp *rxtypes.TraceResponse) {
	if w.dispatcher == nil {
		return
	}
	if resp != nil {
		w.request.OnComplete(resp)
	}
	w.dispatcher.Close()
	w.dispatcher.Wait()
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
//
// The rule for the first positional: it is the pattern only when no
// pattern came with -e/--regexp and --signatures was not given. With
// --signatures a search can have no pattern at all (`rx --signatures
// app.log` only scans for signatures), so a word in the first place
// cannot be told from a path by its look; it is always a path, and a
// pattern goes with -e. `rx --signatures=x ERROR app.log` therefore
// reads ERROR as a file, and fails naming it when there is none.
func resolveTracePositionals(p traceParams) ([]string, []string, error) {
	// Copy --regexp values first (they become the base pattern list).
	patterns := append([]string{}, p.regexps...)
	explicit := append([]string{}, p.paths...)

	// If no --regexp was given, the first positional is the pattern,
	// unless --signatures makes every positional a path.
	args := p.args
	if len(patterns) == 0 && len(args) > 0 && p.signatures == nil {
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
// the paths to search, the temporary file stdin was spooled to ("" when
// stdin carried nothing) and a cleanup function when stdin was read, or
// a nil cleanup when there was nothing to read.
//
// "-" anywhere in the list means "read stdin here"; an empty list with a
// pipe on stdin means the same. Stdin that carries nothing falls back to
// the current directory, which is what rx-python does. ctx ends the
// read: an interrupt while stdin is copied returns ctx's error with the
// spool file removed.
func spoolStdinFor(ctx context.Context, filePaths []string) ([]string, string, func(), error) {
	named := false
	for _, p := range filePaths {
		if p == "-" {
			named = true
			break
		}
	}
	piped := len(filePaths) == 0 && stdinIsPipe()
	if !named && !piped {
		return filePaths, "", nil, nil
	}

	spooled, cleanup, err := spoolStdin(ctx)
	if err != nil {
		return nil, "", nil, err
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
	return out, spooled, cleanup, nil
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
//
// The copy runs on a goroutine of its own, because a read of a pipe
// cannot be interrupted: when ctx ends first (SIGINT or SIGTERM), the
// spool file is removed and ctx's error returned at once, and the
// goroutine is left to end when stdin does or the process exits, whose
// exit is what the interrupt asks for. The goroutine owns the file
// handle: it closes it after the copy, so the removal never races a
// write.
func spoolStdin(ctx context.Context) (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "rx_stdin_*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("reading stdin: %w", err)
	}
	remove := func() { _ = os.Remove(tmp.Name()) }

	type copied struct {
		written int64
		err     error
	}
	// Buffered, so the goroutine's send never blocks once the result is
	// abandoned.
	done := make(chan copied, 1)
	go func() {
		written, copyErr := io.Copy(tmp, os.Stdin)
		closeErr := tmp.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		done <- copied{written, copyErr}
	}()

	select {
	case <-ctx.Done():
		remove()
		return "", nil, ctx.Err()
	case c := <-done:
		if c.err != nil {
			remove()
			return "", nil, fmt.Errorf("reading stdin: %w", c.err)
		}
		if c.written == 0 {
			remove()
			return "", func() {}, nil
		}
		return tmp.Name(), remove, nil
	}
}

// checkTraceCounts refuses a count below 0 with a usage error (exit 2).
// The flags that take a count of lines or matches are listed with the
// value given; each is 0 or more: 0 means no context, or no cap for
// --max-results. A negative count has no meaning, and used to be read
// as an empty context window or as "no cap" without a word. The flags
// not given hold their default, 0, so they never trip the check.
func checkTraceCounts(p traceParams) error {
	counts := []struct {
		flag  string
		value int
	}{
		{"--context", p.ctxLines},
		{"--before", p.beforeCtx},
		{"--after", p.afterCtx},
		{"--max-results", p.maxResults},
	}
	for _, c := range counts {
		if c.value < 0 {
			return exitWithError(os.Stderr, ExitUsageError, "%s must be 0 or more, got %d", c.flag, c.value)
		}
	}
	return nil
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
// asked for — the context section built by internal/output. A trace
// with signatures adds the findings after them (output.FormatFindingsCLI),
// their counts in signatureOrder, the selection's order.
func writeTraceHuman(out io.Writer, resp *rxtypes.TraceResponse, p traceParams, signatureOrder []string) error {
	colorize, err := shouldColorize(p.colorFlag, out)
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}
	_, _ = fmt.Fprint(out, output.FormatTraceCLI(resp, traceFormatOptions(p, colorize)))
	_, _ = fmt.Fprint(out, output.FormatFindingsCLI(resp, output.FindingsFormatOptions{Order: signatureOrder, Colorize: colorize}))
	return nil
}

// traceFormatOptions are the human output's settings for a run: the
// context window, whether the context section is shown, and color.
func traceFormatOptions(p traceParams, colorize bool) output.TraceFormatOptions {
	return output.TraceFormatOptions{
		Before:      resolveBefore(p),
		After:       resolveAfter(p),
		ShowContext: p.showSamples || p.ctxSet || p.beforeSet || p.afterSet,
		Colorize:    colorize,
	}
}

// addDeprecatedDebugFlag keeps `--debug` on the command line of
// `rx trace` although it never did anything: a script that passes it
// keeps working. MarkDeprecated hides the flag from help, and cobra
// prints a one-line deprecation note to stderr when it is used, so
// stdout still carries only the answer. The value is never read.
func addDeprecatedDebugFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("debug", false, "Has no effect")
	// MarkDeprecated fails only for a flag name that does not exist,
	// and the flag is declared on the line above.
	_ = cmd.Flags().MarkDeprecated("debug", "it has no effect and will be removed")
}
