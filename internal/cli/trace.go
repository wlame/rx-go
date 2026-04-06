package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/config"
	"github.com/wlame/rx/internal/engine"
	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/hooks"
	"github.com/wlame/rx/internal/models"
)

// newTraceCommand creates the "trace" subcommand that searches files for regex patterns.
//
// This is the default command — bare `rx PATTERN FILE` is routed here by the root
// command's default routing logic. It supports -- passthrough to rg, stdin input,
// JSON output, inline samples, debug mode, and webhook hooks.
func newTraceCommand() *cobra.Command {
	var (
		outputJSON    bool
		noColor       bool
		debug         bool
		maxResults    int
		samples       bool
		beforeContext int
		afterContext  int
		contextLines  int
		noCache       bool
		noIndex       bool
		onFileURL     string
		onMatchURL    string
		onCompleteURL string
	)

	cmd := &cobra.Command{
		Use:   "trace PATTERN [PATH...] [flags] [-- RG_EXTRA_ARGS...]",
		Short: "Search files for regex patterns using ripgrep",
		Long: `Trace files and directories for regex patterns using ripgrep.

Usage: rx [OPTIONS] PATTERN [PATH ...]

If PATH is not specified, searches in the current directory.
Use '-' as PATH or pipe input to search stdin.

Ripgrep Passthrough:
  Everything after -- is passed directly to ripgrep:
    rx "error" file.log -- -i --case-sensitive

Examples:
  rx "error.*" file.log
  rx "error" --max-results=10
  rx "error" --samples --context=5
  rx "error" --json
  cat file.log | rx "error"`,
		// ArgsFunction: cobra.MinimumNArgs(1) is too strict because we allow
		// pattern-less invocations when stdin is piped. Instead, validate manually.
		Args: cobra.ArbitraryArgs,
		// Cobra's TraverseChildren lets -- passthrough work: everything after --
		// ends up in cmd.ArgsLenAtDash() so we can extract rg extra args.
		TraverseChildren: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrace(cmd, args, traceFlags{
				outputJSON:    outputJSON,
				noColor:       noColor,
				debug:         debug,
				maxResults:    maxResults,
				samples:       samples,
				beforeContext: beforeContext,
				afterContext:  afterContext,
				contextLines:  contextLines,
				noCache:       noCache,
				noIndex:       noIndex,
				onFileURL:     onFileURL,
				onMatchURL:    onMatchURL,
				onCompleteURL: onCompleteURL,
			})
		},
	}

	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output results as JSON")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable debug mode (creates .debug_* files)")
	cmd.Flags().IntVarP(&maxResults, "max-results", "m", 0, "Maximum number of results to return")
	cmd.Flags().BoolVarP(&samples, "samples", "s", false, "Show context lines around matches")
	cmd.Flags().IntVarP(&beforeContext, "before-context", "B", -1, "Lines of context before match (for --samples)")
	cmd.Flags().IntVarP(&afterContext, "after-context", "A", -1, "Lines of context after match (for --samples)")
	cmd.Flags().IntVar(&contextLines, "context", -1, "Lines of context both sides (for --samples)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "Disable trace cache")
	cmd.Flags().BoolVar(&noIndex, "no-index", false, "Disable file index usage")
	cmd.Flags().StringVar(&onFileURL, "on-file-url", "", "Webhook URL called when file scan completes")
	cmd.Flags().StringVar(&onMatchURL, "on-match-url", "", "Webhook URL called for each match")
	cmd.Flags().StringVar(&onCompleteURL, "on-complete-url", "", "Webhook URL called when trace completes")

	// Allow unknown flags to pass through to rg.
	cmd.FParseErrWhitelist.UnknownFlags = true

	return cmd
}

// traceFlags groups all flag values so they can be passed cleanly to runTrace.
type traceFlags struct {
	outputJSON    bool
	noColor       bool
	debug         bool
	maxResults    int
	samples       bool
	beforeContext int
	afterContext  int
	contextLines  int
	noCache       bool
	noIndex       bool
	onFileURL     string
	onMatchURL    string
	onCompleteURL string
}

// runTrace is the core trace execution logic.
func runTrace(cmd *cobra.Command, args []string, flags traceFlags) error {
	// Separate pattern, paths, and rg extra args from the combined args list.
	// Everything after -- (ArgsLenAtDash) is rg passthrough.
	var rgExtraArgs []string
	dashIdx := cmd.ArgsLenAtDash()
	if dashIdx >= 0 {
		rgExtraArgs = args[dashIdx:]
		args = args[:dashIdx]
	}

	if len(args) == 0 {
		// Check for piped stdin with no args at all.
		if !isStdinPiped() {
			return cmd.Help()
		}
	}

	// First arg is the pattern, rest are paths.
	var pattern string
	var paths []string
	if len(args) > 0 {
		pattern = args[0]
		paths = args[1:]
	}

	if pattern == "" && !isStdinPiped() {
		return fmt.Errorf("pattern is required")
	}

	// Handle stdin: explicit "-" path or piped input with no paths.
	var stdinTempFile string
	useStdin := false

	for i, p := range paths {
		if p == "-" {
			useStdin = true
			paths = append(paths[:i], paths[i+1:]...)
			break
		}
	}
	if !useStdin && len(paths) == 0 && isStdinPiped() {
		useStdin = true
	}

	if useStdin {
		tmpFile, err := writeStdinToTemp()
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		stdinTempFile = tmpFile
		defer os.Remove(stdinTempFile)
		paths = append(paths, stdinTempFile)
	}

	// Default to current directory if no paths specified.
	if len(paths) == 0 {
		paths = []string{"."}
	}

	// Apply env var overrides for cache/index flags.
	cfg := config.Load()
	if !flags.noCache && cfg.NoCache {
		flags.noCache = true
	}
	if !flags.noIndex && cfg.NoIndex {
		flags.noIndex = true
	}

	// Calculate context parameters when --samples is requested.
	beforeCtx := 0
	afterCtx := 0
	if flags.samples {
		beforeCtx = resolveContext(flags.beforeContext, flags.contextLines, 3)
		afterCtx = resolveContext(flags.afterContext, flags.contextLines, 3)
	}

	// Debug mode: set RX_DEBUG so the engine logs rg commands.
	if flags.debug {
		os.Setenv("RX_DEBUG", "1")
	}

	// Build and execute trace request.
	reqID := uuid.New().String()

	// Merge request-provided hook URLs with env-configured hooks.
	// Request hooks take priority unless RX_DISABLE_CUSTOM_HOOKS is set.
	requestHooks := hooks.HookCallbacks{
		OnFileScanned: flags.onFileURL,
		OnMatchFound:  flags.onMatchURL,
		OnComplete:    flags.onCompleteURL,
	}
	envHooks := hooks.HookCallbacks{
		OnFileScanned: cfg.HookOnFileURL,
		OnMatchFound:  cfg.HookOnMatchURL,
		OnComplete:    cfg.HookOnCompleteURL,
	}
	effectiveHooks := hooks.GetEffectiveHooks(requestHooks, envHooks, cfg.DisableCustomHooks)

	req := engine.TraceRequest{
		Paths:         paths,
		Patterns:      []string{pattern},
		MaxResults:    flags.maxResults,
		RgExtraArgs:   rgExtraArgs,
		ContextBefore: beforeCtx,
		ContextAfter:  afterCtx,
		UseCache:      !flags.noCache,
		UseIndex:      !flags.noIndex,
		Hooks:         &effectiveHooks,
		RequestID:     reqID,
	}

	startTime := time.Now()
	resp, err := engine.Trace(context.Background(), req)
	if err != nil {
		// Make CLI error messages actionable.
		errMsg := err.Error()
		if strings.Contains(errMsg, "invalid regex") {
			return fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
		if strings.Contains(errMsg, "path not found") {
			return fmt.Errorf("file or directory not found: %w", err)
		}
		return fmt.Errorf("search failed: %w", err)
	}
	elapsed := time.Since(startTime).Seconds()

	resp.RequestID = reqID
	resp.Time = elapsed

	// Fetch inline samples if --samples was requested.
	if flags.samples && len(resp.Matches) > 0 {
		ctxLines := fetchInlineSamples(resp, beforeCtx, afterCtx)
		resp.ContextLines = &ctxLines
		bc := beforeCtx
		ac := afterCtx
		resp.BeforeCtx = &bc
		resp.AfterCtx = &ac
	}

	// Output.
	colorOn := shouldColorize(flags.noColor)
	if flags.outputJSON {
		return outputTraceJSON(cmd.OutOrStdout(), resp)
	}
	outputTraceCLI(cmd.OutOrStdout(), resp, colorOn, pattern, flags.samples, beforeCtx, afterCtx)
	return nil
}

// fetchInlineSamples calls fileutil.GetContext for each match to build context_lines.
func fetchInlineSamples(resp *models.TraceResponse, before, after int) map[string][]models.ContextLine {
	ctxLines := make(map[string][]models.ContextLine)

	for _, m := range resp.Matches {
		filePath, ok := resp.Files[m.File]
		if !ok {
			continue
		}

		key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)
		lines, err := fileutil.GetContext(filePath, int64(m.Offset), before, after)
		if err != nil || len(lines) == 0 {
			continue
		}

		var contextLines []models.ContextLine
		for i, line := range lines {
			contextLines = append(contextLines, models.ContextLine{
				RelativeLineNumber: i + 1,
				AbsoluteLineNumber: -1,
				LineText:           line,
				AbsoluteOffset:     m.Offset,
			})
		}
		ctxLines[key] = contextLines
	}

	return ctxLines
}

// outputTraceJSON marshals the response as indented JSON.
func outputTraceJSON(w io.Writer, resp *models.TraceResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// outputTraceCLI writes human-readable trace output.
func outputTraceCLI(w io.Writer, resp *models.TraceResponse, colorOn bool, pattern string, showSamples bool, beforeCtx, afterCtx int) {
	cfg := config.Load()

	// Summary line.
	totalMatches := len(resp.Matches)
	totalFiles := len(resp.Files)
	fmt.Fprintf(w, "%d matches across %d files (%.3fs)\n",
		totalMatches, totalFiles, resp.Time)

	if totalMatches == 0 {
		return
	}

	// When --samples is active and context lines are available, show context blocks.
	if showSamples && resp.ContextLines != nil {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Samples (context: %d before, %d after):\n\n", beforeCtx, afterCtx)

		for _, m := range resp.Matches {
			filePath := resp.Files[m.File]
			patternText := resp.Patterns[m.Pattern]
			key := fmt.Sprintf("%s:%s:%d", m.Pattern, m.File, m.Offset)

			ctxLines, ok := (*resp.ContextLines)[key]
			if !ok || len(ctxLines) == 0 {
				continue
			}

			header := formatContextHeader(filePath, m.AbsoluteLineNumber, m.Offset, patternText, colorOn)
			fmt.Fprintln(w, header)

			for _, cl := range ctxLines {
				line := processNewlineSymbol(cl.LineText, cfg.NewlineSymbol)
				line = highlightPattern(line, patternText, colorOn)
				fmt.Fprintln(w, line)
			}
			fmt.Fprintln(w)
		}
		return
	}

	// Standard match output — one line per match.
	fmt.Fprintln(w)
	for _, m := range resp.Matches {
		filePath := resp.Files[m.File]
		patternText := resp.Patterns[m.Pattern]
		lineText := ""
		if m.LineText != nil {
			lineText = *m.LineText
		}
		lineText = processNewlineSymbol(lineText, cfg.NewlineSymbol)
		lineNum := m.AbsoluteLineNumber
		if lineNum < 0 && m.RelativeLineNumber != nil {
			lineNum = *m.RelativeLineNumber
		}
		if lineNum < 0 {
			lineNum = 0
		}

		fmt.Fprintln(w, formatMatchLine(filePath, lineNum, lineText, patternText, colorOn))
	}
}

// resolveContext picks the best context value from explicit, symmetric, or default.
func resolveContext(explicit, symmetric, defaultVal int) int {
	if explicit >= 0 {
		return explicit
	}
	if symmetric >= 0 {
		return symmetric
	}
	return defaultVal
}

// isStdinPiped returns true when stdin is not a terminal (i.e. data is piped in).
func isStdinPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// writeStdinToTemp reads all of stdin into a temporary file and returns its path.
func writeStdinToTemp() (string, error) {
	f, err := os.CreateTemp("", "rx_stdin_*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, os.Stdin); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// containsDash checks if a string slice contains "-".
func containsDash(ss []string) bool {
	return strings.Contains(strings.Join(ss, "\x00"), "-")
}
