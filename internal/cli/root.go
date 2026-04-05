package cli

import (
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"
)

// version is injected at build time via -ldflags. Commands that need it
// (e.g. --version) read from this package-level variable.
var version = "dev"

// SetVersion allows the main package to inject the build-time version string.
func SetVersion(v string) { version = v }

// knownSubcommands lists all registered subcommand names. The root command
// uses this to decide whether to route bare args to the trace command.
var knownSubcommands = []string{"trace", "check", "compress", "index", "samples", "serve"}

// NewRootCommand builds and returns the top-level cobra command with all
// subcommands registered. The caller (cmd/rx/main.go) just needs to call Execute().
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "rx",
		Short: "rx -- fast, parallel regex search over compressed and uncompressed files",
		Long: `RX (Regex Tracer) - High-performance file tracing and analysis tool.

Commands:
  rx <pattern> [path ...]   Trace files for patterns (default command)
  rx index <path>           Create/manage file indexes (use --analyze for full analysis)
  rx check <pattern>        Analyze regex complexity
  rx compress <path>        Create seekable zstd for optimized access
  rx samples <path>         Get file content around byte offsets
  rx serve                  Start web API server

Examples:
  rx "error" /var/log/app.log
  rx "error.*failed" /var/log/ -i
  rx index /var/log/app.log --analyze
  rx check "(a+)+"
  rx serve --port=8000

For more help on each command:
  rx --help
  rx index --help
  rx check --help
  rx serve --help`,
		// SilenceErrors / SilenceUsage prevent cobra from printing its own
		// error messages — we handle error display ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		// Version is printed by --version flag.
		Version: version,
	}

	// Override cobra's default version template to match Python: "RX <version>".
	root.SetVersionTemplate(fmt.Sprintf("RX %s\n", version))

	// Register all subcommands.
	root.AddCommand(
		newTraceCommand(),
		newCheckCommand(),
		newCompressCommand(),
		newIndexCommand(),
		newSamplesCommand(),
		newServeCommand(),
	)

	return root
}

// Execute builds the root command and runs it. This is the single entry point
// called from cmd/rx/main.go.
func Execute() {
	root := NewRootCommand()

	// Default command routing: if the first positional arg is not a known
	// subcommand (and not a flag like --help / --version), prepend "trace"
	// so that bare `rx PATTERN FILE` works like `rx trace PATTERN FILE`.
	//
	// This mirrors the Python DefaultCommandGroup.parse_args behavior.
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {}
	originalArgs := os.Args[1:]
	if shouldRouteToTrace(originalArgs) {
		os.Args = append([]string{os.Args[0], "trace"}, originalArgs...)
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

// shouldRouteToTrace decides whether the argument list should be routed to the
// trace subcommand. This handles three cases:
//  1. First arg is a non-flag, non-subcommand (e.g., `rx "error" file.log`)
//  2. First arg is a flag like --json followed by a pattern (e.g., `rx --json "error" file.log`)
//  3. --help / --version → do NOT route to trace
func shouldRouteToTrace(args []string) bool {
	if len(args) == 0 {
		return false
	}

	// If first arg is a known subcommand, no routing needed.
	if slices.Contains(knownSubcommands, args[0]) {
		return false
	}

	// If first arg is not a flag, it's a pattern — route to trace.
	if !isFlag(args[0]) {
		return true
	}

	// First arg is a flag. If it's --help or --version, don't route.
	if args[0] == "--help" || args[0] == "-h" || args[0] == "--version" {
		return false
	}

	// First arg is a flag (e.g., --json, --no-color). Check if there's a
	// non-flag arg later that is NOT a known subcommand — that means the
	// user typed something like `rx --json "error" file.log`.
	for _, a := range args {
		if !isFlag(a) {
			if slices.Contains(knownSubcommands, a) {
				return false // Found a subcommand — cobra will handle it.
			}
			return true // Found a non-flag, non-subcommand arg — route to trace.
		}
	}

	return false
}

// isFlag returns true if the argument looks like a CLI flag (starts with -).
func isFlag(arg string) bool {
	return len(arg) > 0 && arg[0] == '-'
}
