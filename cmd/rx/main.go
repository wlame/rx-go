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
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/clicommand"

	// Blank-import analyzer detectors so their package init() calls
	// register them with the global analyzer registry before main runs
	// analyzer.Freeze(). Add one line per detector as we roll out the
	// MVP catalog (see docs/plans/2026-04-21-analyzers.md).
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/jsonblob"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/longline"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/repeatidentical"
	_ "github.com/wlame/rx-go/internal/analyzer/detectors/tracebackjava"
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
	root := newRootCmd()
	args := preprocessArgs(os.Args[1:])
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		// cobra already prints its own error; we just need a non-zero exit.
		os.Exit(1)
	}
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

// writeErr keeps error output consistent across main paths.
func writeErr(format string, a ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", a...)
}

var _ = writeErr // reserved for future use
