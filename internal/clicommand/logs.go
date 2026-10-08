package clicommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewLogsCommand builds `rx logs`, the parent of the commands that read
// a rotated log — `syslog`, `syslog.1`, `syslog.2.gz`, … — as one log
// chain. Each subcommand calls the same function as its /v1/logs route.
//
//	rx logs list /var/log             # the chains of a directory
//	rx logs show /var/log/syslog      # one chain: its parts in time order, checks, state
//	rx logs time-range /var/log/syslog  # its first and last timestamp
//	rx logs index /var/log/syslog     # build and store every part's line index
//
// `rx logs` alone prints its help; an unknown subcommand is a usage
// error.
func NewLogsCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Read rotated logs (syslog, syslog.1, syslog.2.gz, …) as one log chain",
		Long: "A log chain is the files of one rotated log in one directory, found by their names: " +
			"the active file (syslog) and its numbered or dated parts (syslog.1, syslog.2.gz, " +
			"syslog-20261001.gz). The subcommands read a chain as one text.",
		// NoArgs makes a word that names no subcommand a usage error
		// ("unknown command"), where cobra would otherwise print the help
		// and exit 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newLogsListCommand(out), newLogsShowCommand(out), newLogsTimeRangeCommand(out),
		newLogsIndexCommand(out))
	return cmd
}

// newLogsListCommand builds `rx logs list`: the chains of each
// directory, as GET /v1/logs/chains gives them.
func newLogsListCommand(out io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list [DIR...]",
		Short: "List the log chains of each directory (default: the current one), found by file name",
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"."}
			}
			return runLogsList(out, args, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false,
		"Output JSON: one object for one directory, an array of objects for several")
	return cmd
}

// runLogsList answers every directory in order. A directory it cannot
// list is reported on stderr and the others are still answered; the
// exit code is then that directory's (multiPathFailure), as
// `rx time-range` does for several paths.
func runLogsList(out io.Writer, dirs []string, jsonOutput bool) error {
	answers := make([]*rxtypes.ChainsResponse, 0, len(dirs))
	var failureCodes []int
	var single *ExitError
	for _, dir := range dirs {
		resp, err := logchain.List(dir)
		if err != nil {
			failure := logsListFailure(dir, err)
			failureCodes = append(failureCodes, failure.Code)
			single = failure
			continue
		}
		answers = append(answers, resp)
	}
	if err := writeChainLists(out, answers, len(dirs) == 1, jsonOutput); err != nil {
		return err
	}
	if len(dirs) == 1 && single != nil {
		return single
	}
	return multiPathFailureWith(failureCodes, logsListFailureSummaries, "one or more directories could not be listed")
}

// logsListFailureSummaries is the closing error line of an
// `rx logs list` run over several directories whose every failure had
// the same exit code.
var logsListFailureSummaries = map[int]string{
	ExitFileNotFound: "one or more directories do not exist",
	ExitAccessDenied: "one or more directories were outside the search roots or could not be read",
}

// logsListFailure prints why a directory cannot be listed and returns
// the exit code for it: 4 outside the search roots, into a hidden entry
// or not readable, 3 when it does not exist, 2 for a file.
func logsListFailure(dir string, err error) *ExitError {
	var outside *paths.ErrPathOutsideRoots
	var hidden *paths.ErrHiddenPath
	switch {
	case errors.As(err, &outside), errors.As(err, &hidden):
		return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
	case errors.Is(err, logchain.ErrNotADirectory):
		return exitWithError(os.Stderr, ExitUsageError, "path is not a directory: %s", dir)
	case errors.Is(err, fs.ErrNotExist):
		return exitWithError(os.Stderr, ExitFileNotFound, "directory not found: %s", dir)
	}
	return openFailure(dir, err)
}

// writeChainLists prints the answers: as JSON, one object when one
// directory was given and an array otherwise; or a table per directory.
func writeChainLists(out io.Writer, answers []*rxtypes.ChainsResponse, oneDir, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if oneDir {
			if len(answers) == 0 {
				return nil
			}
			return enc.Encode(answers[0])
		}
		return enc.Encode(answers)
	}
	for i, resp := range answers {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		writeChainTable(out, resp)
	}
	return nil
}

// maxMissingShown is how many missing part names a table row shows
// before it gives the count of the rest.
const maxMissingShown = 3

// writeChainTable prints one directory's chains: a line naming the
// directory, then one row per chain with its name, its number of parts,
// their total size, whether every frozen part is indexed, and the
// missing parts.
//
//	/var/log: 2 chains
//	NAME      PARTS  SIZE      IDX  MISSING
//	dpkg.log  12     24.31 KB  -    -
//	syslog    8      24.79 KB  idx  syslog.3
func writeChainTable(out io.Writer, resp *rxtypes.ChainsResponse) {
	_, _ = fmt.Fprintf(out, "%s: %s\n", output.Printable(resp.Path), chainCount(len(resp.Chains)))
	if len(resp.Chains) == 0 {
		return
	}
	// Go note: a tabwriter buffers the rows and pads each tab-separated
	// cell to its column's width when Flush is called.
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPARTS\tSIZE\tIDX\tMISSING")
	for _, c := range resp.Chains {
		idx := "-"
		if c.IsIndexed {
			idx = "idx"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			output.Printable(c.Name), partsCell(c), output.HumanSize(c.Size), idx, missingCell(c.Missing, c.MissingCount))
	}
	_ = tw.Flush()
}

// partsCell is the PARTS cell of a row: how many parts the chain has,
// and how many of them cannot be read ("8 (1 unreadable)"), or ">10000"
// for a chain of more than logchain.MaxParts parts, whose answer lists
// none.
func partsCell(c rxtypes.ChainEntry) string {
	if c.TooManyParts {
		return ">" + strconv.Itoa(logchain.MaxParts)
	}
	cell := strconv.Itoa(len(c.Parts))
	if len(c.Unreadable) > 0 {
		cell += fmt.Sprintf(" (%d unreadable)", len(c.Unreadable))
	}
	return cell
}

// chainCount words how many chains a directory holds.
func chainCount(n int) string {
	if n == 1 {
		return "1 chain"
	}
	return strconv.Itoa(n) + " chains"
}

// missingCell is the MISSING cell of a row: "-" for none, the names up
// to maxMissingShown, and how many more are missing after them. count
// is the answer's missing_count, the number of missing parts in all:
// the answer names at most logchain.MaxMissingNames of them, so the
// rest is counted from it, not from the names.
func missingCell(missing []string, count int) string {
	if len(missing) == 0 {
		return "-"
	}
	shown := missing
	if len(shown) > maxMissingShown {
		shown = shown[:maxMissingShown]
	}
	names := make([]string, len(shown))
	for i, name := range shown {
		names[i] = output.Printable(name)
	}
	cell := strings.Join(names, ", ")
	if rest := count - len(shown); rest > 0 {
		cell += fmt.Sprintf(" and %d more", rest)
	}
	return cell
}
