package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
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
// Exactly one of --offsets / --lines is required. // rework: both modes now dispatch to internal/samples.Resolve which is
// shared with the HTTP handler — no more divergence between CLI and
// HTTP behavior.
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
		offsets    []string
		lines      []string
		ctxLines   int
		beforeCtx  int
		afterCtx   int
		jsonOutput bool
		colorFlag  string // "", "always", "never"
		noColor    bool
		regex      string
		noIndex    bool
	)
	cmd := &cobra.Command{
		Use:   "samples PATH",
		Short: "Get context lines around byte offsets or line numbers in a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --no-color is the Python-compat
			// bool flag — when set it overrides --color.
			if noColor {
				colorFlag = "never"
			}
			return runSamples(out, samplesParams{
				path:      args[0],
				offsets:   offsets,
				lines:     lines,
				ctxLines:  ctxLines,
				beforeCtx: beforeCtx,
				afterCtx:  afterCtx,
				// --before=0 asks for no lines before, which is not the
				// same as leaving the flag out; only Changed() can tell
				// the two apart on an int flag.
				beforeSet:  cmd.Flags().Changed("before"),
				afterSet:   cmd.Flags().Changed("after"),
				jsonOutput: jsonOutput,
				colorFlag:  colorFlag,
				regex:      regex,
				noIndex:    noIndex || config.GetBoolEnv("RX_NO_INDEX", false),
			})
		},
	}
	// Python-compatible short aliases:
	//   -b → --offsets, -l → --lines, -c → --context, -r → --regex
	//   -B → --before (already Python), -A → --after (already Python)
	//   --no-color → suppress ANSI output
	// Both spellings of "several positions" work: a comma-separated list
	// in one flag, and the flag repeated. rx-python takes the repeated
	// form and this took the comma-separated one, so a command that named
	// several positions ran against exactly one backend — and the
	// repeated form here silently kept only the last value, which is the
	// worse of the two failures.
	cmd.Flags().StringArrayVarP(&offsets, "offsets", "b", nil,
		"Byte offsets or ranges; comma-separated, or repeat the flag")
	cmd.Flags().StringArrayVarP(&lines, "lines", "l", nil,
		"1-based line numbers or ranges; comma-separated, or repeat the flag")
	// rx-python spells these --byte-offset and --line-offset. Both
	// spellings work in both backends so a command written for either one
	// runs on the other, which is what the drop-in-replacement contract
	// asks for. The aliases are hidden so --help stays one name per flag.
	cmd.Flags().StringArrayVar(&offsets, "byte-offset", nil, "Alias for --offsets (rx-python spelling)")
	cmd.Flags().StringArrayVar(&lines, "line-offset", nil, "Alias for --lines (rx-python spelling)")
	_ = cmd.Flags().MarkHidden("byte-offset")
	_ = cmd.Flags().MarkHidden("line-offset")
	cmd.Flags().IntVarP(&ctxLines, "context", "c", 3, "Context lines before AND after")
	cmd.Flags().IntVarP(&beforeCtx, "before", "B", 0, "Override lines before")
	cmd.Flags().IntVarP(&afterCtx, "after", "A", 0, "Override lines after")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")
	cmd.Flags().StringVar(&colorFlag, "color", "auto",
		"Colorize output: 'always', 'never', or 'auto' (color only on a terminal)")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output (Python-compat alias for --color=never)")
	cmd.Flags().StringVarP(&regex, "regex", "r", "", "Highlight matches of this regex in context lines (requires color)")
	cmd.Flags().BoolVar(&noIndex, "no-index", false,
		"Do not build or use a line index (also RX_NO_INDEX)")
	return cmd
}

type samplesParams struct {
	path      string
	offsets   []string
	lines     []string
	ctxLines  int
	beforeCtx int
	afterCtx  int
	// beforeSet and afterSet say whether --before / --after were given;
	// a given value, 0 included, overrides --context.
	beforeSet  bool
	afterSet   bool
	jsonOutput bool
	colorFlag  string
	regex      string
	noIndex    bool
}

// runSamples dispatches the CLI request to the shared samples.Resolve
// implementation. replaces the divergent
// in-line implementation with the shared resolver.
func runSamples(out io.Writer, p samplesParams) error {
	// Mode mutual exclusion.
	if (len(p.offsets) == 0) == (len(p.lines) == 0) {
		return exitWithError(os.Stderr, ExitUsageError,
			"must provide exactly one of --offsets or --lines")
	}

	// Sandbox + stat.
	_, err := paths.ValidatePathWithinRoots(p.path)
	if err != nil && !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
		return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
	}
	info, err := os.Stat(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return exitWithError(os.Stderr, ExitFileNotFound, "file not found: %s", p.path)
		}
		return exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
	}
	if info.IsDir() {
		return exitWithError(os.Stderr, ExitUsageError, "path is a directory, not a file: %s", p.path)
	}

	// Parse the spec string into OffsetOrRange slices.
	var (
		parsedOffsets []samples.OffsetOrRange
		parsedLines   []samples.OffsetOrRange
	)
	if len(p.offsets) > 0 {
		parsedOffsets, err = samples.ParseCSV(strings.Join(p.offsets, ","))
	} else {
		parsedLines, err = samples.ParseCSV(strings.Join(p.lines, ","))
	}
	if err != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	}

	// Context precedence: a given --before/--after overrides --context,
	// as before_context/after_context do over HTTP.
	before := p.ctxLines
	if p.beforeSet {
		before = p.beforeCtx
	}
	after := p.ctxLines
	if p.afterSet {
		after = p.afterCtx
	}

	// A second lookup in a multi-gigabyte file is the case an index
	// exists for, so one is built when the file is worth it and none is
	// cached. `--no-index` opts out for a caller that wants the read to
	// leave nothing behind. rx-python builds on the same path, and the
	// answer is identical either way — an index only changes how fast it
	// is reached.
	//
	// The CLI waits for the build however long it takes: a command has
	// no client to hand a task to, and `rx serve` answers 202 only
	// because an HTTP request should not hang for minutes.
	//
	// When the cache cannot store the index (a read-only cache, or
	// RX_CACHE_DIR naming a regular file), nothing is built: a build
	// would read the whole file for an index nobody keeps, on every
	// call. ShouldBuildIndex logs the cause once and the lookup reads
	// the file without an index. A build that fails anyway is logged
	// and the lookup goes on: the index is an accelerator, the answer
	// is the same without it, and refusing to read a file because its
	// index could not be written would be the wrong trade.
	// What the file is, decided once through its pin before any index
	// is built for it. A file whose text is not text has no lines to
	// give: it is refused, like a directory, with the reason.
	source, err := paths.Pin(p.path)
	if err != nil {
		return exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
	}
	kind, err := samples.Classify(samples.Request{Source: source})
	if errors.Is(err, filekind.ErrNotText) {
		return exitWithError(os.Stderr, ExitUsageError, "%s: %s", err.Error(), p.path)
	}
	if err != nil {
		return exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
	}

	if !p.noIndex && samples.ShouldBuildIndex(p.path, kind, info.Size()) {
		if _, _, buildErr := samples.BuildIndex(p.path, nil); buildErr != nil {
			slog.Default().Warn("index_not_built", "path", p.path, "error", buildErr.Error())
		}
	}

	// The loader hands the resolver the stored index for its
	// index-aware seeks. A missing, stale or damaged index is an absent
	// one and the resolver reads the file from the start instead.
	// `--no-index` (or RX_NO_INDEX) reads no index file at all: it is
	// the escape hatch for a caller who suspects the index, so it must
	// not consult the very file it is meant to avoid.
	loader := samples.StoredIndex
	if p.noIndex {
		loader = samples.NoIndex
	}

	req := samples.Request{
		Path:          p.path,
		Source:        source,
		Kind:          &kind,
		Offsets:       parsedOffsets,
		Lines:         parsedLines,
		BeforeContext: before,
		AfterContext:  after,
		IndexLoader:   loader,
	}
	resp, err := samples.Resolve(req)
	if err != nil {
		return exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
	}

	// A position with no line behind it is answered as -1, and a person
	// reading the terminal is told why. It goes to stderr so --json
	// output stays parseable, and it is emitted for both output modes:
	// a script redirecting stdout still sees the reason.
	//
	// The command still succeeds. The other positions in the same
	// request were answered, and throwing them away over one bad number
	// would make a batch useless.
	warnAboutMissingPositions(os.Stderr, resp)

	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	colorize, colorErr := shouldColorize(p.colorFlag, out)
	if colorErr != nil {
		return exitWithError(os.Stderr, ExitUsageError, "%s", colorErr.Error())
	}
	rendered := output.FormatSamplesCLI(resp, colorize, p.regex)
	_, _ = fmt.Fprintln(out, rendered)
	return nil
}

// shouldColorize decides whether to emit ANSI codes, given the --color
// value and the writer. Semantics:
//
//	"always"       → always color, even into a pipe
//	"never"        → never color
//	"auto" or ""   → color only when `out` is a terminal, and not when
//	                 NO_COLOR or RX_NO_COLOR is set
//
// "" is the historical spelling of "auto" and stays accepted. Anything
// else is a usage error rather than a silent fall back to auto: a typo
// that quietly does the opposite of what was asked is worse than a
// refusal, and rx-python's click.Choice refuses it too.
//
// --color=always wins over NO_COLOR and RX_NO_COLOR, which is what GNU
// ls and most modern tools do — the flag is the more specific
// instruction.
func shouldColorize(flag string, out io.Writer) (bool, error) {
	switch flag {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto", "":
		return colorDecision(false, out), nil
	}
	return false, fmt.Errorf("--color must be 'always', 'never' or 'auto', got %q", flag)
}

// warnAboutMissingPositions names every requested position the file does
// not have — a line past the last one, line 0, or a byte offset past the
// last byte.
//
// The answer already says so — -1 in the number map, null in samples —
// but a person reading the terminal should not have to know that
// convention to understand why a line came back empty. rx-python prints
// the same sentence.
func warnAboutMissingPositions(w io.Writer, resp *rxtypes.SamplesResponse) {
	for _, key := range sortedPositionKeys(resp.Lines) {
		if resp.Lines[key] == -1 && !strings.Contains(key, "-") {
			_, _ = fmt.Fprintf(w, "Warning: line %s is not in the file.\n", key)
		}
	}
	for _, key := range sortedPositionKeys(resp.Offsets) {
		if resp.Offsets[key] == -1 && !strings.Contains(key, "-") {
			_, _ = fmt.Fprintf(w, "Warning: offset %s is not in the file.\n", key)
		}
	}
}

// sortedPositionKeys orders the keys numerically, so the warnings come
// out in the order a reader expects rather than in map order.
func sortedPositionKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aErr := strconv.ParseInt(strings.SplitN(keys[i], "-", 2)[0], 10, 64)
		b, bErr := strconv.ParseInt(strings.SplitN(keys[j], "-", 2)[0], 10, 64)
		if aErr == nil && bErr == nil && a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}
