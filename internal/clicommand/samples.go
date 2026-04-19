package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewSamplesCommand builds the `rx samples` cobra command.
//
// Shape matches rx-python/src/rx/cli/samples.py (samples_command):
//
//	rx samples PATH -l 100 -c 3                   # single line
//	rx samples PATH -b 5000 --context=5           # byte offset
//	rx samples PATH --lines=200-350,450-600       # multi-range
//	rx samples PATH --lines=-1 --before=2 --after=5
//	rx samples PATH --lines=100 --regex 'error.*' --color=always
//
// Exactly one of --offsets / --lines is required. Stage 9 Round 2
// rework: both modes now dispatch to internal/samples.Resolve which is
// shared with the HTTP handler — no more divergence between CLI and
// HTTP behavior (R1-B4 / R1-B5 root cause).
//
// Colored output:
//
//	--color       force ANSI color / "always" / "never"
//	--no-color    Python-compat alias for --color=never
//	(default)     autodetect: color if stdout is a TTY
//
// Regex highlighting (--regex) wraps each match with bright-red escape
// codes when colors are active; it's ignored when output is plain.
func NewSamplesCommand(out io.Writer) *cobra.Command {
	var (
		offsets    string
		lines      string
		ctxLines   int
		beforeCtx  int
		afterCtx   int
		jsonOutput bool
		colorFlag  string // "", "always", "never"
		noColor    bool
		regex      string
	)
	cmd := &cobra.Command{
		Use:   "samples PATH",
		Short: "Get context lines around byte offsets or line numbers in a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stage 9 Round 2 R1-B11: --no-color is the Python-compat
			// bool flag — when set it overrides --color.
			if noColor {
				colorFlag = "never"
			}
			return runSamples(out, samplesParams{
				path:       args[0],
				offsets:    offsets,
				lines:      lines,
				ctxLines:   ctxLines,
				beforeCtx:  beforeCtx,
				afterCtx:   afterCtx,
				jsonOutput: jsonOutput,
				colorFlag:  colorFlag,
				regex:      regex,
			})
		},
	}
	// Python-compatible short aliases (Stage 9 Round 2 R1-B11):
	//   -b → --offsets, -l → --lines, -c → --context, -r → --regex
	//   -B → --before (already Python), -A → --after (already Python)
	//   --no-color → suppress ANSI output
	cmd.Flags().StringVarP(&offsets, "offsets", "b", "", "Comma-separated byte offsets or ranges")
	cmd.Flags().StringVarP(&lines, "lines", "l", "", "Comma-separated 1-based line numbers or ranges")
	cmd.Flags().IntVarP(&ctxLines, "context", "c", 3, "Context lines before AND after")
	cmd.Flags().IntVarP(&beforeCtx, "before", "B", 0, "Override lines before")
	cmd.Flags().IntVarP(&afterCtx, "after", "A", 0, "Override lines after")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")
	cmd.Flags().StringVar(&colorFlag, "color", "", "Colorize output: 'always', 'never', or '' (auto)")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output (Python-compat alias for --color=never)")
	cmd.Flags().StringVarP(&regex, "regex", "r", "", "Highlight matches of this regex in context lines (requires color)")
	return cmd
}

type samplesParams struct {
	path       string
	offsets    string
	lines      string
	ctxLines   int
	beforeCtx  int
	afterCtx   int
	jsonOutput bool
	colorFlag  string
	regex      string
}

// runSamples dispatches the CLI request to the shared samples.Resolve
// implementation. Stage 9 Round 2 U rework: replaces the divergent
// in-line implementation with the shared resolver.
func runSamples(out io.Writer, p samplesParams) error {
	// Mode mutual exclusion.
	if (p.offsets == "") == (p.lines == "") {
		_ = exitWithError(os.Stderr, ExitUsageError,
			"must provide exactly one of --offsets or --lines")
		return errors.New("mutex")
	}

	// Sandbox + stat.
	_, err := paths.ValidatePathWithinRoots(p.path)
	if err != nil && !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
		_ = exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
		return err
	}
	info, err := os.Stat(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			_ = exitWithError(os.Stderr, ExitFileNotFound, "file not found: %s", p.path)
			return err
		}
		_ = exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
		return err
	}
	if info.IsDir() {
		_ = exitWithError(os.Stderr, ExitUsageError, "path is a directory, not a file: %s", p.path)
		return errors.New("is directory")
	}

	// Parse the spec string into OffsetOrRange slices.
	var (
		parsedOffsets []samples.OffsetOrRange
		parsedLines   []samples.OffsetOrRange
	)
	if p.offsets != "" {
		parsedOffsets, err = samples.ParseCSV(p.offsets)
	} else {
		parsedLines, err = samples.ParseCSV(p.lines)
	}
	if err != nil {
		_ = exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
		return err
	}

	// Context precedence: explicit --before/--after override --context.
	before := p.beforeCtx
	if before == 0 {
		before = p.ctxLines
	}
	after := p.afterCtx
	if after == 0 {
		after = p.ctxLines
	}

	// IndexLoader hooks up the cached unified index for index-aware
	// line-offset seeks. index.LoadForSource returns
	// (nil, ErrIndexNotFound) when no cache exists, (nil, nil) when
	// stale, and (idx, nil) when valid. The resolver treats
	// (nil, nil) as "no index — fall back to linear scan", so we
	// swallow the not-found error to match that contract and keep
	// "index missing" non-fatal.
	loader := func(path string) (*rxtypes.UnifiedFileIndex, error) {
		idx, loadErr := index.LoadForSource(path)
		if loadErr != nil {
			// Missing cache is not an error for samples; any other
			// error (permission, IO) propagates so the user sees it.
			if errors.Is(loadErr, index.ErrIndexNotFound) {
				return nil, nil
			}
			return nil, loadErr
		}
		return idx, nil
	}

	req := samples.Request{
		Path:          p.path,
		Offsets:       parsedOffsets,
		Lines:         parsedLines,
		BeforeContext: before,
		AfterContext:  after,
		IndexLoader:   loader,
	}
	resp, err := samples.Resolve(req)
	if err != nil {
		_ = exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
		return err
	}

	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	colorize := shouldColorize(p.colorFlag, out)
	rendered := output.FormatSamplesCLI(resp, colorize, p.regex)
	_, _ = fmt.Fprintln(out, rendered)
	return nil
}

// shouldColorize returns true if ANSI codes should be emitted for the
// given flag value and writer. Semantics:
//
//	"always" → always color (unless RX_NO_COLOR / NO_COLOR set)
//	"never"  → never color
//	""       → auto: color if `out` is os.Stdout / os.Stderr attached to
//	           a TTY. For non-*os.File writers (typical in tests),
//	           return false so golden-file comparisons are plain text
//	           unless --color=always is passed.
//
// Env overrides NO_COLOR and RX_NO_COLOR disable color unconditionally —
// this matches rx-python's colorDecision. --color=always can still
// override env when explicitly set, matching most modern CLI tools
// (e.g. GNU ls --color=always).
func shouldColorize(flag string, out io.Writer) bool {
	switch flag {
	case "always":
		return true
	case "never":
		return false
	}
	// Auto-detect path honors NO_COLOR / RX_NO_COLOR.
	return colorDecision(false, out)
}
