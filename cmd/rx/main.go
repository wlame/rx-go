// Package main is the rx CLI binary.
//
// Typical usage:
//
//	rx "pattern" /var/log/app.log            # trace is the default command
//	rx trace "pattern" /var/log/app.log      # explicit trace
//	rx samples /var/log/app.log --lines=100
//	rx index /var/log/app.log
//	rx compress /var/log/app.log
//	rx signatures                            # the signatures rx can find
//	rx serve --port=7777
//
// The "default subcommand is trace" behavior is implemented by
// preprocessArgs: if the first non-flag argument isn't a known
// subcommand, we inject "trace" before it so cobra routes correctly.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/clicommand"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/sigscan"
)

// appVersion is overridden at link time via -ldflags "-X main.appVersion=..."
// The default "dev" is used when the binary is built without ldflags.
var appVersion = "dev"

// knownSubcommands is the flat list of top-level subcommand names
// preprocessArgs recognizes. "help" and "completion" are the commands
// cobra generates; the others are registered in newRootCmd. "version"
// is no command (cobra makes only the --version flag, and `rx version`
// exits 2 with "unknown command"), but it stays in the list so that a
// bare `rx version` is not a trace of the pattern "version" waiting on
// standard input. Any first positional argument not in this set falls
// through to the default, trace.
//
// A []string + slices.Contains is used instead of map[string]bool
// because the list is tiny (~8 entries) and the slice form reads more
// naturally as "the registered commands".
var knownSubcommands = []string{
	"trace",
	"samples",
	"time-range",
	"logs",
	"index",
	"scan",
	"compress",
	"signatures",
	"serve",
	"help",
	"completion",
	"version",
}

func main() {
	root := newRootCmd()
	args := preprocessArgs(os.Args[1:])
	root.SetArgs(args)

	// SIGINT / SIGTERM cancel the command's context so the engine winds
	// down its rg subprocesses (and a trace reading standard input drops
	// its spool), then the process exits 5. signal.NotifyContext keeps
	// catching the two signals until stop() runs, so the goroutine calls
	// it as soon as the first signal lands: the default handling is back
	// during the shutdown, and a second Ctrl-C kills the process at once
	// instead of being swallowed until the command returns. stop is safe
	// to call more than once; main calls it again below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := root.ExecuteContext(ctx)
	// Read the context before stop(): stop() is NotifyContext's cancel
	// func, so calling it first would make every run look interrupted.
	// No `defer stop()` here — os.Exit below would skip it, and the
	// explicit call covers every path out of this function.
	interrupted := ctx.Err() != nil
	stop()
	// Commands print their own failures; what reaches here unprinted is
	// cobra's own (an unknown flag, a missing argument) or an error built
	// outside a command, such as a search root that does not exist. An
	// interrupt prints nothing: the user already knows why it stopped.
	if err != nil && !interrupted && !clicommand.IsReported(err) {
		clicommand.PrintError(os.Stderr, err.Error())
	}
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
	// cobra's own flag-parsing failures are usage errors; main prints
	// their message, since SilenceErrors keeps cobra from doing it.
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
			"  rx logs list /var/log              # rotated logs, one chain each\n" +
			"  rx index file.log                  # build a line index\n" +
			"  rx scan --recursive /var/log       # cache the signature findings of every file\n" +
			"  rx compress file.log               # make a seekable .zst\n" +
			"  rx signatures                      # list the signatures rx can find\n" +
			"  rx serve --port=7777               # HTTP API + rx-viewer SPA",
		SilenceUsage:  true, // don't dump usage on runtime errors
		SilenceErrors: true, // we print errors ourselves
		Version:       appVersion,
	}
	root.SetVersionTemplate("rx version {{.Version}}\n")
	// The version flag is defined here, without a shorthand: cobra's
	// own would add -v, which the routing never leaves to cobra and
	// which, in ripgrep, inverts a match. The help lists --version only,
	// and `rx -v` stays a usage error.
	root.Flags().Bool("version", false, "version for rx")

	// Hidden entries — names starting with a dot — are skipped by
	// default, as ripgrep skips them. This is persistent rather than
	// per-command because it is one policy: every subcommand that walks
	// a directory or validates a path honors it.
	var includeHidden bool
	root.PersistentFlags().BoolVar(&includeHidden, "hidden", config.GetBoolEnv("RX_HIDDEN", false),
		"Include hidden files and directories (names starting with a dot)")

	// The --search-root sandbox is persistent for the same reason: a
	// confined rx is a property of the process, not of one subcommand.
	// `serve` declares its own --search-root because its default is the
	// current directory rather than "no sandbox"; a local flag shadows
	// the persistent one, so the two never both bind.
	var searchRoots []string
	root.PersistentFlags().StringArrayVar(&searchRoots, "search-root", nil,
		"Restrict file access to this directory (repeatable; default: no sandbox)")

	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		// RX_LOG_LEVEL is applied first, so even the warnings the other
		// settings write obey it.
		config.ApplyLogLevel()
		paths.SetIncludeHidden(includeHidden)
		return installSearchRoots(searchRoots)
	}

	root.AddCommand(clicommand.NewTraceCommand(os.Stdout))
	root.AddCommand(clicommand.NewSamplesCommand(os.Stdout))
	root.AddCommand(clicommand.NewTimeRangeCommand(os.Stdout))
	root.AddCommand(clicommand.NewLogsCommand(os.Stdout))
	root.AddCommand(clicommand.NewIndexCommand(os.Stdout))
	root.AddCommand(clicommand.NewScanCommand(os.Stdout))
	root.AddCommand(clicommand.NewCompressCommand(os.Stdout))
	// The scan engine runs every signature's examples while the
	// signatures load: a signature whose examples fail is refused. The
	// loader takes the checker as a function, since the engine imports
	// the loader's package.
	root.AddCommand(clicommand.NewSignaturesCommand(os.Stdout, sigscan.CheckExamples))
	root.AddCommand(clicommand.NewServeCommand(os.Stdout, appVersion))
	return root
}

// installSearchRoots applies the --search-root sandbox for this process.
//
// The flag wins over RX_SEARCH_ROOTS, which is how `rx serve` passes the
// sandbox down to any subprocess it spawns. With neither set there is no
// sandbox at all: every path check reports ErrNoSearchRootsConfigured and
// the commands treat that as "allow", which is what an ordinary CLI
// invocation relies on.
//
// A root that does not exist is a usage error rather than a silent
// no-sandbox: a caller who asked to be confined and was not would never
// find out.
func installSearchRoots(flagRoots []string) error {
	roots := flagRoots
	if len(roots) == 0 {
		roots = config.GetPathSepEnv("RX_SEARCH_ROOTS")
	}
	if len(roots) == 0 {
		return nil
	}
	if err := paths.SetSearchRoots(roots); err != nil {
		return clicommand.NewExitError(clicommand.ExitUsageError,
			fmt.Errorf("search roots: %w", err))
	}
	return nil
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

// metaFlags are the root flags cobra answers by itself, wherever they
// stand among the leading flags: `rx --hidden --version` prints the
// version as `rx --version` does.
var metaFlags = []string{"--help", "-h", "--version"}

// rootFlagsWithValue are the root flags that take a separate value when
// written without "=": the walk steps over the value, so that in
// `rx --search-root DIR serve` the word after the flag is its value and
// `serve` is the subcommand.
var rootFlagsWithValue = []string{"--search-root"}

// shouldRouteToTrace decides whether the argument list should have "trace"
// prepended. Four cases:
//
//  1. len(args) == 0 → false (bare `rx` prints help).
//  2. args[0] is a known subcommand → false (cobra dispatches directly).
//  3. A meta flag (--help, -h, --version) among the leading flags →
//     false (cobra handles it).
//  4. Otherwise → route iff the first non-flag arg is not a subcommand
//     (meaning it's a pattern for `rx trace`).
//
// Case 4 handles leading-flag forms like `rx --json "error" file.log` by
// walking forward past the leading flags, and past the value of a root
// flag that takes one, to find the first positional.
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
	// Cases 3 and 4b: leading flags. A meta flag among them is cobra's;
	// otherwise the first non-flag arg decides. If it's a subcommand,
	// leave argv alone (the flags belong to that subcommand via cobra's
	// parser). If it's a pattern, route to trace.
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !isFlag(a) {
			return !slices.Contains(knownSubcommands, a)
		}
		if slices.Contains(metaFlags, a) {
			return false
		}
		if slices.Contains(rootFlagsWithValue, a) {
			// The next word is the flag's value, not a positional.
			i++
		}
	}
	// All args are flags with no positional (e.g. bare `rx --json`). We
	// route to trace so cobra surfaces a consistent "missing pattern"
	// usage error from the trace command rather than a root-level error.
	return true
}

// isFlag returns true if the argument looks like a CLI flag (starts with -).
// An empty string is not a flag. This intentionally matches both long-form
// ("--json") and short-form ("-j") flags.
func isFlag(arg string) bool {
	return len(arg) > 0 && arg[0] == '-'
}
