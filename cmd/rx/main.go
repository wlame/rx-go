// Package main is the rx CLI binary.
//
// Typical usage:
//
//	rx "pattern" /var/log/app.log            # trace is the default command
//	rx trace "pattern" /var/log/app.log      # explicit trace
//	rx samples /var/log/app.log --lines=100
//	rx index /var/log/app.log --analyze
//	rx compress /var/log/app.log
//	rx serve --port=7777
//
// The "default subcommand is trace" behavior is implemented by
// preprocessArgs: if the first non-flag argument isn't a known
// subcommand, we inject "trace" before it so cobra routes correctly.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/paths"

	// Blank-import analyzer detectors so their package init() calls
	// register them with the global analyzer registry before main runs
	// analyzer.Freeze(). One line per detector; the catalog is described
	// in docs/concepts/analyzers.md.
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/coredumpunix"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/jsonblob"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/longline"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/repeatidentical"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/secretsscan"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackgo"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjava"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjs"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackpython"
)

// appVersion is overridden at link time via -ldflags "-X main.appVersion=..."
// The default "dev" is used when the binary is built without ldflags.
var appVersion = "dev"

// knownSubcommands is the flat list of top-level subcommand names
// preprocessArgs recognizes. "help", "completion", and "version" are
// cobra-generated; the others are explicitly registered in newRootCmd.
// Any first-positional arg not in this set falls through to the
// default (trace).
//
// A []string + slices.Contains is used instead of map[string]bool
// because the list is tiny (~8 entries) and the slice form reads more
// naturally as "the registered commands". Pattern borrowed from
// another-rx-go/internal/cli/root.go.
var knownSubcommands = []string{
	"trace",
	"samples",
	"index",
	"compress",
	"serve",
	"help",
	"completion",
	"version",
}

func main() {
	// Freeze the analyzer registry before any request can reach it. Go
	// guarantees every package's init() has completed before main() runs,
	// so by this point all blank-imported detector packages have called
	// RegisterLineDetector. After Freeze, any further Register* call
	// panics (a defensive signal to catch misuse during development).
	analyzer.Freeze()

	root := newRootCmd()
	args := preprocessArgs(os.Args[1:])
	root.SetArgs(args)

	// SIGINT / SIGTERM cancel the command's context so the engine winds
	// down its rg subprocesses, then the process exits 5. signal.NotifyContext
	// restores the default handler when stop() runs, so a second Ctrl-C
	// during shutdown still kills the process immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := root.ExecuteContext(ctx)
	// Read the context before stop(): stop() is NotifyContext's cancel
	// func, so calling it first would make every run look interrupted.
	// No `defer stop()` here — os.Exit below would skip it, and the
	// explicit call covers every path out of this function.
	interrupted := ctx.Err() != nil
	stop()
	os.Exit(exitCodeFor(err, interrupted))
}

// exitCodeFor maps the error cobra returned to a process exit code.
//
//	nil                      → 0
//	*clicommand.ExitError    → its Code
//	anything else            → 1
//
// An interrupt wins over everything: when the user signaled us, whatever
// error the command reported downstream is a consequence of the signal,
// and the contract says 5.
func exitCodeFor(err error, interrupted bool) int {
	if interrupted {
		return clicommand.ExitInterrupted
	}
	if err == nil {
		return clicommand.ExitSuccess
	}
	var exitErr *clicommand.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	// cobra's own flag-parsing failures are usage errors; it has already
	// printed the message.
	if isUsageError(err) {
		return clicommand.ExitUsageError
	}
	return clicommand.ExitGenericError
}

// isUsageError recognizes the errors cobra and pflag produce for a bad
// command line. They are plain fmt.Errorf values with no type to match
// on, so the text is all there is to go by.
func isUsageError(err error) bool {
	msg := err.Error()
	for _, prefix := range []string{
		"unknown flag",
		"unknown shorthand flag",
		"unknown command",
		"flag needs an argument",
		"invalid argument",
		"accepts ",
		"requires at least",
	} {
		if strings.Contains(msg, prefix) {
			return true
		}
	}
	return false
}

// newRootCmd assembles every subcommand. The root command's own RunE is
// empty; `rx --help` falls through to cobra's default help rendering.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "rx",
		Short: "High-performance regex tracer + file indexer for large logs",
		Long: "rx (Regex Tracer) searches huge log files faster than plain ripgrep by " +
			"chunking work across cores and caching match results.\n\n" +
			"Common flows:\n" +
			"  rx \"pattern\" file.log              # trace (default)\n" +
			"  rx samples file.log --lines=100    # context around line 100\n" +
			"  rx index file.log --analyze        # build anomaly index\n" +
			"  rx compress file.log               # make a seekable .zst\n" +
			"  rx serve --port=7777               # HTTP API + rx-viewer SPA",
		SilenceUsage:  true, // don't dump usage on runtime errors
		SilenceErrors: true, // we print errors ourselves
		Version:       appVersion,
	}
	root.SetVersionTemplate("rx version {{.Version}}\n")

	// Hidden entries — names starting with a dot — are skipped by
	// default, as ripgrep skips them. This is persistent rather than
	// per-command because it is one policy: every subcommand that walks
	// a directory or validates a path honors it.
	var includeHidden bool
	root.PersistentFlags().BoolVar(&includeHidden, "hidden", config.GetBoolEnv("RX_HIDDEN", false),
		"Include hidden files and directories (names starting with a dot)")
	root.PersistentPreRun = func(_ *cobra.Command, _ []string) {
		paths.SetIncludeHidden(includeHidden)
	}

	root.AddCommand(clicommand.NewTraceCommand(os.Stdout))
	root.AddCommand(clicommand.NewSamplesCommand(os.Stdout))
	root.AddCommand(clicommand.NewIndexCommand(os.Stdout))
	root.AddCommand(clicommand.NewCompressCommand(os.Stdout))
	root.AddCommand(clicommand.NewServeCommand(os.Stdout, appVersion))
	return root
}

// preprocessArgs rewrites os.Args[1:] so that bare `rx "pattern" file.log`
// invocations are interpreted as `rx trace "pattern" file.log`. If the user
// already supplied a known subcommand (or --help / -h / --version), argv is
// returned unchanged.
//
// This preserves the "click DefaultCommandGroup"–style default-subcommand
// behavior from the original Python CLI. The routing decision is split into
// shouldRouteToTrace (pure predicate) so it's easy to unit-test.
func preprocessArgs(args []string) []string {
	if shouldRouteToTrace(args) {
		// Prepend "trace" so cobra dispatches to the trace subcommand.
		return append([]string{"trace"}, args...)
	}
	return args
}

// shouldRouteToTrace decides whether the argument list should have "trace"
// prepended. Four cases:
//
//  1. len(args) == 0 → false (bare `rx` prints help).
//  2. args[0] is a known subcommand → false (cobra dispatches directly).
//  3. args[0] is --help / -h / --version → false (cobra handles meta flags).
//  4. Otherwise → route iff the first non-flag arg is not a subcommand
//     (meaning it's a pattern for `rx trace`).
//
// Case 4 handles leading-flag forms like `rx --json "error" file.log` by
// walking forward past the leading flags to find the first positional.
//
// Borrowed from another-rx-go/internal/cli/root.go:96-139.
func shouldRouteToTrace(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// Case 2: first arg is a registered subcommand — cobra routes itself.
	if slices.Contains(knownSubcommands, args[0]) {
		return false
	}
	// Case 4a: first arg is a plain positional (e.g. `rx error file.log`).
	// That "error" is a pattern, so route to trace.
	if !isFlag(args[0]) {
		return true
	}
	// Case 3: top-level meta flags — leave cobra to print help/version.
	if args[0] == "--help" || args[0] == "-h" || args[0] == "--version" {
		return false
	}
	// Case 4b: first arg is some other flag (e.g. --json). Walk forward:
	// the first non-flag arg decides. If it's a subcommand, leave argv
	// alone (the flags belong to that subcommand via cobra's parser). If
	// it's a pattern, route to trace.
	for _, a := range args {
		if !isFlag(a) {
			return !slices.Contains(knownSubcommands, a)
		}
	}
	// All args are flags with no positional (e.g. bare `rx --json`). We
	// route to trace so cobra surfaces a consistent "missing pattern"
	// usage error from the trace command rather than a root-level error.
	// This preserves the pre-refactor rx-go behavior; the reference
	// another-rx-go returns false here, but we keep ours to avoid a
	// user-facing regression.
	return true
}

// isFlag returns true if the argument looks like a CLI flag (starts with -).
// An empty string is not a flag. This intentionally matches both long-form
// ("--json") and short-form ("-j") flags.
func isFlag(arg string) bool {
	return len(arg) > 0 && arg[0] == '-'
}
