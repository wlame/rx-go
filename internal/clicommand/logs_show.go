package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fingerprintPattern is what a chain's fingerprint looks like: 16
// lowercase hex digits.
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// newLogsShowCommand builds `rx logs show`: the description of each
// chain, as GET /v1/logs/chain gives it, read without waiting for any
// background work (a part without a line index is indexed in memory).
func newLogsShowCommand(out io.Writer) *cobra.Command {
	var (
		jsonOutput  bool
		fileTZ      string
		fingerprint string
	)
	cmd := &cobra.Command{
		Use:   "show CHAIN...",
		Short: "Describe each log chain: its parts in time order, line counts, times, state and checks",
		Long: "CHAIN is a chain's handle: its directory joined with its name (/var/log/syslog), as rx logs list " +
			"gives it. Parts without a line index are read in full (the indexes are not stored; rx logs index " +
			"stores them). Exit 3 when a handle names no chain, 6 when a chain is invalid (after printing it), " +
			"7 when --fingerprint= differs from the chain's.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			fileZone, err := parseFileTZ(fileTZ)
			if err != nil {
				return err
			}
			if err := checkFingerprintFlag(fingerprint, len(args)); err != nil {
				return err
			}
			return runLogsShow(out, args, logsShowOptions{json: jsonOutput, fileZone: fileZone, fingerprint: fingerprint})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false,
		"Output JSON: one object for one chain, an array of objects for several")
	cmd.Flags().StringVar(&fileTZ, "file-tz", "", fileTZFlagUsage)
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "",
		"The fingerprint of an earlier description (16 hex digits): exit 7 when the chain's files changed since")
	return cmd
}

// logsShowOptions are the flags of `rx logs show`.
type logsShowOptions struct {
	json        bool
	fileZone    config.Zone
	fingerprint string
}

// checkFingerprintFlag refuses a --fingerprint= that is not 16 hex
// digits, or that is given with several chains (a fingerprint belongs to
// one chain).
func checkFingerprintFlag(fingerprint string, chains int) error {
	if fingerprint == "" {
		return nil
	}
	if !fingerprintPattern.MatchString(fingerprint) {
		return exitWithError(os.Stderr, ExitUsageError, "invalid --fingerprint=%q: give the 16 hex digits of a chain's fingerprint", fingerprint)
	}
	if chains != 1 {
		return exitWithError(os.Stderr, ExitUsageError, "--fingerprint= names one chain's files; give one CHAIN with it")
	}
	return nil
}

// runLogsShow describes every chain in order and prints them. A handle
// it cannot describe is reported on stderr and the others are still
// described. The exit code is the one failure's for one chain, and
// multiPathFailureWith's for several: 6 for an invalid chain and 7 for a
// changed fingerprint count as failures, after the chain is printed.
func runLogsShow(out io.Writer, handles []string, opts logsShowOptions) error {
	answers := make([]*rxtypes.ChainResponse, 0, len(handles))
	var failureCodes []int
	var single *ExitError
	for _, handle := range handles {
		resp, failure := showChain(handle, opts)
		if resp != nil {
			answers = append(answers, resp)
		}
		if failure != nil {
			failureCodes = append(failureCodes, failure.Code)
			single = failure
		}
	}
	if err := writeChainDescriptions(out, answers, len(handles) == 1, opts); err != nil {
		return err
	}
	if len(handles) == 1 && single != nil {
		return single
	}
	return multiPathFailureWith(failureCodes, logsShowFailureSummaries, "one or more chains could not be described or are invalid")
}

// logsShowFailureSummaries is the closing error line of a run over
// several chains whose every failure had the same exit code.
var logsShowFailureSummaries = map[int]string{
	ExitFileNotFound: "one or more handles name no log chain",
	ExitAccessDenied: "one or more chains were outside the search roots or could not be read",
	ExitChainInvalid: "one or more chains are invalid",
	ExitChainChanged: "the chain's files changed",
}

// showChain describes one chain for `rx logs show`. It returns the
// description (nil when there is none to print) and the failure the
// command exits with for it: the description's own (invalid, changed
// fingerprint), printed on stderr after a reason, or the error that kept
// it from being described.
func showChain(handle string, opts logsShowOptions) (*rxtypes.ChainResponse, *ExitError) {
	d, failure := describeForCLI(handle, opts.fileZone)
	if failure != nil {
		return nil, failure
	}
	resp := d.Response
	resp.CLICommand = webapi.BuildCLICommand("log_chain", map[string]any{
		"path": resp.Path, "file_tz": opts.fileZone.Name, "fingerprint": opts.fingerprint,
	})
	return resp, chainFailure(resp, opts.fingerprint)
}

// describeForCLI resolves and describes a chain the way the CLI reads
// one: every part read now, an unindexed part indexed in memory
// (Options.Scan). The failure is printed on stderr.
func describeForCLI(handle string, fileZone config.Zone) (*logchain.Description, *ExitError) {
	d, _, err := logchain.DescribeHandle(context.Background(), handle, logchain.Options{FileZone: fileZone, Scan: true})
	if err != nil {
		return nil, logChainFailure(handle, err)
	}
	return d, nil
}

// logChainFailure prints why a chain cannot be described and returns the
// exit code for it: 2 for a handle that ends in no name, 3 when it names
// no chain or its directory does not exist, 4 outside the search roots,
// into a hidden entry or not readable, 7 when the chain's files kept
// changing while they were read.
func logChainFailure(handle string, err error) *ExitError {
	var outside *paths.ErrPathOutsideRoots
	var hidden *paths.ErrHiddenPath
	switch {
	case errors.Is(err, logchain.ErrInvalidHandle):
		return exitWithError(os.Stderr, ExitUsageError, "%s: %s", err.Error(), handle)
	case errors.As(err, &outside), errors.As(err, &hidden):
		return exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
	case errors.Is(err, logchain.ErrNotAChain):
		return exitWithError(os.Stderr, ExitFileNotFound, "not a log chain (fewer than two parts): %s", handle)
	case errors.Is(err, fs.ErrNotExist):
		return exitWithError(os.Stderr, ExitFileNotFound, "directory not found: %s", handle)
	case errors.Is(err, logchain.ErrPartChanged):
		return exitWithError(os.Stderr, ExitChainChanged, "the files of the chain kept changing while they were read: %s", handle)
	}
	return openFailure(handle, err)
}

// chainFailure is the exit a described chain gives: 7 when the
// fingerprint given differs from the chain's, 6 when the chain is
// invalid, none otherwise. The reason is printed on stderr.
func chainFailure(resp *rxtypes.ChainResponse, fingerprint string) *ExitError {
	if fingerprint != "" && fingerprint != resp.Fingerprint {
		return exitWithError(os.Stderr, ExitChainChanged, "the files of %s changed: fingerprint %s, given %s",
			output.Printable(resp.Path), resp.Fingerprint, fingerprint)
	}
	if resp.State == rxtypes.ChainStateInvalid {
		return exitWithError(os.Stderr, ExitChainInvalid, "the chain %s is invalid: %s", output.Printable(resp.Path),
			strings.Join(reasonWords(resp.Reasons), "; "))
	}
	return nil
}

// reasonWords are the codes of reasons, for a one-line message.
func reasonWords(reasons []rxtypes.ChainReason) []string {
	words := make([]string, 0, len(reasons))
	for _, r := range reasons {
		words = append(words, r.Code)
	}
	return words
}

// writeChainDescriptions prints the descriptions: as JSON, one object
// when one chain was given and an array otherwise; or a block per chain.
func writeChainDescriptions(out io.Writer, answers []*rxtypes.ChainResponse, oneChain bool, opts logsShowOptions) error {
	if opts.json {
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
	loc := chainDisplayLocation(opts.fileZone)
	for i, resp := range answers {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		writeChainDescription(out, resp, loc)
	}
	return nil
}

// chainDisplayLocation is the zone `rx logs show` prints times in: the
// file zone when one is given, else RX_LOG_TZ (UTC by default).
func chainDisplayLocation(fileZone config.Zone) *time.Location {
	if fileZone.Location != nil {
		return fileZone.Location
	}
	if loc := config.LogTZ().Location; loc != nil {
		return loc
	}
	return time.UTC
}

// chainTimeLayout is how `rx logs show` writes a time.
const chainTimeLayout = "2006-01-02 15:04:05.000"

// writeChainDescription prints one chain: a line with its handle, state,
// part and line counts and fingerprint; the reasons of an invalid chain;
// a table of its parts in order; then its gaps and missing parts.
//
//	/var/log/syslog: ready, 3 parts, 240 lines, fingerprint 3fa2c4d5e6f70812, times in UTC
//	#  NAME                           COMPRESSION  LINES  GLOBAL LINES  FIRST TIME               HIGHEST TIME             IDX
//	1  syslog-20260930-1790726400.gz  gzip         130    1-130         2026-09-29 00:00:01.000  2026-09-30 01:59:58.000  idx
func writeChainDescription(out io.Writer, resp *rxtypes.ChainResponse, loc *time.Location) {
	header := fmt.Sprintf("%s: %s, %s", output.Printable(resp.Path), resp.State, countWord(len(resp.Parts), "part"))
	if resp.LineCount != nil {
		header += ", " + countWord(int(*resp.LineCount), "line")
	}
	_, _ = fmt.Fprintf(out, "%s, fingerprint %s, times in %s\n", header, resp.Fingerprint, loc.String())
	for _, r := range resp.Reasons {
		_, _ = fmt.Fprintf(out, "reason %s: %s\n", r.Code, output.Printable(r.Message))
	}
	// Go note: a tabwriter buffers the rows and pads each tab-separated
	// cell to its column's width when Flush is called.
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "#\tNAME\tCOMPRESSION\tLINES\tGLOBAL LINES\tFIRST TIME\tHIGHEST TIME\tIDX")
	for i, p := range resp.Parts {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, output.Printable(p.Name),
			compressionCell(p), countCell(p.LineCount), globalLinesCell(p), timeCell(p.FirstMs, false, loc),
			timeCell(p.MaxMs, p.MaxIsBound, loc), indexCell(p))
	}
	_ = tw.Flush()
	for _, g := range resp.Gaps {
		_, _ = fmt.Fprintf(out, "gap: no lines from %s to %s (after %s, before %s)\n",
			timeCell(&g.FromMs, false, loc), timeCell(&g.ToMs, false, loc), output.Printable(g.After), output.Printable(g.Before))
	}
	if len(resp.Missing) > 0 {
		_, _ = fmt.Fprintf(out, "missing: %s\n", missingCell(resp.Missing))
	}
}

// countWord writes a count with its noun, plural when it is not one.
func countWord(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// compressionCell is a part's compression, or "-" for a plain file.
func compressionCell(p rxtypes.ChainPart) string {
	if p.CompressionFormat == nil {
		return "-"
	}
	return *p.CompressionFormat
}

// countCell is a count, or "?" when it is not known.
func countCell(n *int64) string {
	if n == nil {
		return unknownTime
	}
	return strconv.FormatInt(*n, 10)
}

// globalLinesCell is the range of global line numbers a part holds:
// "1-130", "-" for an empty part, "131-" for the active file whose count
// is not known, "?" before the chain is ready.
func globalLinesCell(p rxtypes.ChainPart) string {
	switch {
	case p.GlobalStart == nil:
		return unknownTime
	case p.LineCount == nil:
		return strconv.FormatInt(*p.GlobalStart, 10) + "-"
	case *p.LineCount == 0:
		return "-"
	}
	return fmt.Sprintf("%d-%d", *p.GlobalStart, *p.GlobalStart+*p.LineCount-1)
}

// timeCell writes an instant in loc, "<=" before an upper bound, or "?"
// when it is not known.
func timeCell(ms *int64, bound bool, loc *time.Location) string {
	if ms == nil {
		return unknownTime
	}
	text := time.UnixMilli(*ms).In(loc).Format(chainTimeLayout)
	if bound {
		return "<=" + text
	}
	return text
}

// indexCell is "idx" for a part with a current line index, else "-".
func indexCell(p rxtypes.ChainPart) string {
	if p.IsIndexed {
		return "idx"
	}
	return "-"
}

// newLogsTimeRangeCommand builds `rx logs time-range`: the first and the
// last timestamp of each chain, shown as `rx time-range` shows a file's.
func newLogsTimeRangeCommand(out io.Writer) *cobra.Command {
	var jsonOutput bool
	var fileTZ string
	cmd := &cobra.Command{
		Use:   "time-range CHAIN...",
		Short: "Show the first and last timestamp of each log chain",
		Long: "CHAIN is a chain's handle, as for rx logs show. The times are the chain's first and last, known once " +
			"the chain is ready. Exit 3 when a handle names no chain, 6 when a chain is invalid (after printing it).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			fileZone, err := parseFileTZ(fileTZ)
			if err != nil {
				return err
			}
			return runLogsTimeRange(out, args, jsonOutput, fileZone)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false,
		"Output JSON: one object for one chain, an array of objects for several")
	cmd.Flags().StringVar(&fileTZ, "file-tz", "", fileTZFlagUsage)
	return cmd
}

// runLogsTimeRange answers every chain in order, as runLogsShow does.
func runLogsTimeRange(out io.Writer, handles []string, jsonOutput bool, fileZone config.Zone) error {
	answers := make([]*rxtypes.ChainTimeRange, 0, len(handles))
	var failureCodes []int
	var single *ExitError
	for _, handle := range handles {
		d, failure := describeForCLI(handle, fileZone)
		if failure == nil {
			answers = append(answers, chainTimeRange(d.Response, fileZone))
			failure = chainFailure(d.Response, "")
		}
		if failure != nil {
			failureCodes = append(failureCodes, failure.Code)
			single = failure
		}
	}
	if err := writeChainTimeRanges(out, answers, len(handles) == 1, jsonOutput, fileZone); err != nil {
		return err
	}
	if len(handles) == 1 && single != nil {
		return single
	}
	return multiPathFailureWith(failureCodes, logsShowFailureSummaries, "one or more chains could not be described or are invalid")
}

// chainTimeRange is the time range of a described chain: its first and
// last timestamp, and the format and zone of its first part with
// timestamps, which the human line shows the times in.
func chainTimeRange(resp *rxtypes.ChainResponse, fileZone config.Zone) *rxtypes.ChainTimeRange {
	out := &rxtypes.ChainTimeRange{
		Path: resp.Path, Name: resp.Name, State: resp.State, FirstMs: resp.FirstMs, LastMs: resp.LastMs,
		DisplayZone: chainDisplayLocation(fileZone).String(),
		CLICommand:  webapi.BuildCLICommand("logs_time_range", map[string]any{"path": resp.Path, "file_tz": fileZone.Name}),
	}
	for _, p := range resp.Parts {
		if p.TimeFormat != nil {
			format := p.TimeFormat.Format
			out.Format, out.DisplayZone = &format, p.TimeFormat.AssumedZone
			break
		}
	}
	return out
}

// writeChainTimeRanges prints the answers: as JSON, one object when one
// chain was given and an array otherwise; or one line per chain.
func writeChainTimeRanges(out io.Writer, answers []*rxtypes.ChainTimeRange, oneChain, jsonOutput bool, fileZone config.Zone) error {
	if !jsonOutput {
		for _, r := range answers {
			_, _ = fmt.Fprintln(out, chainTimeRangeLine(r, fileZone))
		}
		return nil
	}
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

// chainTimeRangeLine is the human line of one chain, in the layout of
// `rx time-range`: the handle, the format of its first part with
// timestamps, the first and last timestamp, the zone they are shown in,
// and the state. The times are written as `rx logs show` writes them,
// to the millisecond, since the parts of a chain may write theirs in
// several formats.
//
//	/var/log/syslog  syslog  2026-09-29 00:00:01.000 .. 2026-10-07 00:06:03.000  UTC  ready
func chainTimeRangeLine(r *rxtypes.ChainTimeRange, fileZone config.Zone) string {
	handle := output.Printable(r.Path)
	if r.Format == nil {
		return strings.Join([]string{handle, "no timestamps", r.State}, "  ")
	}
	loc := chainDisplayLocation(fileZone)
	if fileZone.Location == nil && r.DisplayZone == "UTC" {
		loc = time.UTC
	}
	span := timeCell(r.FirstMs, false, loc) + " .. " + timeCell(r.LastMs, false, loc)
	return strings.Join([]string{handle, *r.Format, span, output.Printable(r.DisplayZone), r.State}, "  ")
}
