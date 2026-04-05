package cli

import (
	"fmt"
	"io"

	json "github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/wlame/rx/internal/models"
)

// newCheckCommand creates the "check" subcommand — a STUB that returns
// "not yet available" for regex complexity analysis.
//
// The full implementation is deferred to a future phase. The command and JSON
// contract exist now so the CLI surface area is complete and the API contract
// is preserved.
func newCheckCommand() *cobra.Command {
	var (
		outputJSON bool
		noColor    bool
	)

	cmd := &cobra.Command{
		Use:   "check PATTERN",
		Short: "Analyze regex pattern complexity (stub)",
		Long: `Analyze regex pattern complexity and detect ReDoS vulnerabilities.

NOTE: This feature is not yet implemented in the Go version.
The command exists to preserve the CLI contract. It always returns
a stub response with status "not_implemented" and exit code 0.

Examples:
  rx check "error.*"
  rx check "(a+)+" --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			return runCheck(cmd, pattern, checkFlags{
				outputJSON: outputJSON,
				noColor:    noColor,
			})
		},
	}

	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colored output")

	return cmd
}

// checkFlags groups check command flags.
type checkFlags struct {
	outputJSON bool
	noColor    bool
}

// runCheck outputs the stub complexity response. Always returns nil (exit code 0).
func runCheck(cmd *cobra.Command, pattern string, flags checkFlags) error {
	w := cmd.OutOrStdout()

	if flags.outputJSON {
		resp := models.NewStubComplexityResponse(pattern)
		return outputCheckJSON(w, &resp)
	}

	// Text mode: simple message.
	fmt.Fprintln(w, "regex complexity analysis is not yet available")
	return nil
}

// outputCheckJSON marshals the stub complexity response as indented JSON.
func outputCheckJSON(w io.Writer, resp *models.ComplexityResponse) error {
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}
