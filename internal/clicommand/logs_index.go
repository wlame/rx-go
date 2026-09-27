package clicommand

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// newLogsIndexCommand builds `rx logs index`: the line index of every
// part of each chain, built and stored in the foreground, part by part,
// as `rx index` builds one file's, then the chain as `rx logs show`
// prints it.
func newLogsIndexCommand(out io.Writer) *cobra.Command {
	var jsonOutput, force bool
	cmd := &cobra.Command{
		Use:   "index CHAIN...",
		Short: "Build and store the line index of every part of each log chain",
		Long: "CHAIN is a chain's handle, as for rx logs show. Every part is indexed, the active file too, " +
			"whatever its size (RX_LARGE_FILE_MB does not apply); a part whose index is current is kept unless " +
			"--force. Each part is reported as rx index reports a file, then the chain is printed as rx logs show " +
			"prints it. Exit as rx index (1 when a part could not be indexed, 4 when one could not be read), and 3 " +
			"when a handle names no chain.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runLogsIndex(out, args, logsIndexOptions{json: jsonOutput, force: force})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false,
		"Output JSON: one object for one chain, an array of objects for several")
	cmd.Flags().BoolVar(&force, "force", false, "Rebuild the index of every part, current ones too")
	return cmd
}

// logsIndexOptions are the flags of `rx logs index`.
type logsIndexOptions struct {
	json  bool
	force bool
}

// logsIndexAnswer is what `rx logs index --json` prints for one chain:
// the chain's handle, the members of `rx index --json` for its parts
// (indexed, skipped, skip_reasons, errors, total_time), and the chain's
// description after the build, as `rx logs show --json` prints it.
type logsIndexAnswer struct {
	Path string `json:"path"`
	indexBuildResult
	Chain *rxtypes.ChainResponse `json:"chain"`
	// namedParts is how many parts the names of the chain's files give,
	// for its human head (shownChain).
	namedParts int
}

// runLogsIndex indexes every chain in order and prints each. A handle
// that names no chain is reported on stderr and the others are still
// indexed. The exit code is the one failure's for one chain (its
// describe failure, or what `rx index` gives for its parts), and
// multiPathFailureWith's over every failure for several.
func runLogsIndex(out io.Writer, handles []string, opts logsIndexOptions) error {
	answers := make([]logsIndexAnswer, 0, len(handles))
	var failureCodes []int
	var single error
	for _, handle := range handles {
		answer, failure := indexChain(handle, opts.force)
		if answer != nil {
			answers = append(answers, *answer)
			codes := answer.failureCodes()
			failureCodes = append(failureCodes, codes...)
			single = multiPathFailure(codes, "one or more parts failed to index")
		}
		if failure != nil {
			failureCodes = append(failureCodes, failure.Code)
			single = failure
		}
	}
	if err := writeLogsIndex(out, answers, len(handles) == 1, opts.json); err != nil {
		return err
	}
	if len(handles) == 1 {
		return single
	}
	return multiPathFailureWith(failureCodes, logsIndexFailureSummaries, "one or more chains could not be indexed")
}

// logsIndexFailureSummaries is the closing error line of a run over
// several chains whose every failure had the same exit code.
var logsIndexFailureSummaries = map[int]string{
	ExitFileNotFound: "one or more handles name no log chain, or parts do not exist",
	ExitAccessDenied: "one or more chains or parts were outside the search roots or could not be read",
}

// indexChain indexes one chain for `rx logs index`: it describes the
// chain without reading any part in full (as GET /v1/logs/chain does),
// builds and stores the line index of each part in the chain's order
// through the code of `rx index`, with no size threshold, then
// describes the chain again as `rx logs show` does. The answer is nil
// when the chain cannot be described at first; the failure is the
// describe's, printed on stderr.
func indexChain(handle string, force bool) (*logsIndexAnswer, *ExitError) {
	before, _, err := logchain.DescribeHandle(context.Background(), handle, logchain.Options{})
	if err != nil {
		return nil, logChainFailure(handle, err)
	}
	parts := before.Parts()
	partPaths := make([]string, len(parts))
	for i, p := range parts {
		partPaths[i] = p.Path
	}
	// The chain needs every part's line count and times, so no part is
	// skipped for its size: a threshold of 0 indexes every file.
	noThreshold := 0
	answer := &logsIndexAnswer{
		Path:             before.Response.Path,
		indexBuildResult: buildIndexes(indexParams{paths: partPaths, force: force, threshold: &noThreshold}),
	}
	after, failure := describeForCLI(handle, config.Zone{})
	if failure != nil {
		return answer, failure
	}
	answer.Chain, answer.namedParts = after.Response, after.Candidate.NamedParts
	answer.Chain.CLICommand = webapi.BuildCLICommand("log_chain", map[string]any{"path": answer.Chain.Path})
	return answer, nil
}

// writeLogsIndex prints the answers: as JSON, one object when one chain
// was given and an array otherwise; or per chain the lines `rx index`
// prints for its parts, counted as parts, then the chain as
// `rx logs show` prints it.
func writeLogsIndex(out io.Writer, answers []logsIndexAnswer, oneChain, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if oneChain {
			if len(answers) == 0 {
				return nil
			}
			return enc.Encode(answers[0])
		}
		return enc.Encode(answers)
	}
	loc := chainDisplayLocation(config.Zone{})
	for i, answer := range answers {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		writeIndexBuildHuman(out, answer.indexBuildResult, false, indexUnitParts)
		if answer.Chain != nil {
			writeChainDescription(out, shownChain{resp: answer.Chain, namedParts: answer.namedParts}, loc)
		}
	}
	return nil
}
