package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logsSamplesParams are the flags of `rx logs samples`.
type logsSamplesParams struct {
	handle     string
	lines      []string
	part       string
	timestamps []string
	ctxLines   int
	beforeCtx  int
	afterCtx   int
	// beforeSet and afterSet say whether --before / --after were given,
	// as for `rx samples`: a given value, 0 included, overrides
	// --context.
	beforeSet   bool
	afterSet    bool
	jsonOutput  bool
	fileTZ      string
	fingerprint string
}

// newLogsSamplesCommand builds `rx logs samples`: lines of a log chain
// by global line, by part and line, or by time, as GET /v1/logs/samples
// gives them. It never waits for background work: the chain is
// described the way `rx logs show` describes it (a part without a line
// index is indexed in memory), so it is ready unless it is invalid, and
// every part is read with its stored index or without one.
func newLogsSamplesCommand(out io.Writer) *cobra.Command {
	var p logsSamplesParams
	cmd := &cobra.Command{
		Use:   "samples CHAIN",
		Short: "Get lines of a log chain by global line, by part and line, or by time",
		Long: "CHAIN is a chain's handle, as for rx logs show. --lines= takes the chain's global line numbers " +
			"(100, 100-200, -1), or with --part= the part's own numbers as rx samples gives them for that part; " +
			"--timestamps= takes times as rx samples does, the first line in the chain's order at or after each. " +
			"Context crosses part edges. Each line is printed with its global number and its part and local " +
			"number, and a -- NAME -- line where a part's lines start. Exit 3 when the handle names no chain, " +
			"6 when the chain is invalid, 7 when --fingerprint= differs or a part changed while it was read, " +
			"2 for a part that is not a member and the usage errors of rx samples.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.handle = args[0]
			p.beforeSet, p.afterSet = cmd.Flags().Changed("before"), cmd.Flags().Changed("after")
			return runLogsSamples(out, p)
		},
	}
	cmd.Flags().StringArrayVarP(&p.lines, "lines", "l", nil,
		"Global line numbers or ranges (the part's own with --part); comma-separated, or repeat the flag")
	cmd.Flags().StringVar(&p.part, "part", "", "The bare name of a part, whose own numbers --lines= gives")
	cmd.Flags().StringArrayVarP(&p.timestamps, "timestamps", "t", nil,
		"Times or time ranges (T, T1..T2, ..T2, T1..); repeat the flag for several")
	cmd.Flags().IntVarP(&p.ctxLines, "context", "c", 3, "Context lines before AND after")
	cmd.Flags().IntVarP(&p.beforeCtx, "before", "B", 0, "Override lines before")
	cmd.Flags().IntVarP(&p.afterCtx, "after", "A", 0, "Override lines after")
	cmd.Flags().BoolVar(&p.jsonOutput, "json", false, "Output the GET /v1/logs/samples body as JSON")
	cmd.Flags().StringVar(&p.fileTZ, "file-tz", "", fileTZFlagUsage)
	cmd.Flags().StringVar(&p.fingerprint, "fingerprint", "",
		"The fingerprint of an earlier description (16 hex digits): exit 7 when the chain's files changed since")
	return cmd
}

// runLogsSamples answers one chain samples request.
func runLogsSamples(out io.Writer, p logsSamplesParams) error {
	req, fileZone, err := logsSamplesRequestOf(p)
	if err != nil {
		return err
	}
	d, failure := describeForCLI(p.handle, fileZone)
	if failure != nil {
		return failure
	}
	if failure := chainFailure(d.Response, p.fingerprint); failure != nil {
		return failure
	}
	resp, err := logchain.Samples(context.Background(), d, req, readPartForCLI)
	if err != nil {
		return logsSamplesFailure(p.handle, err)
	}
	webapi.FillChainSamplesCommands(resp, p.fileTZ, map[string]any{
		"path": resp.Path, "lines": strings.Join(p.lines, ","), "part": p.part, "timestamps": p.timestamps,
		"file_tz": p.fileTZ, "fingerprint": p.fingerprint, "context": p.ctxLines,
		"before_context": givenInt(p.beforeSet, p.beforeCtx), "after_context": givenInt(p.afterSet, p.afterCtx),
	})
	warnAboutMissingChainPositions(os.Stderr, resp, p.part)
	if p.jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	loc := chainDisplayLocation(fileZone)
	_, _ = fmt.Fprint(out, output.FormatChainSamples(resp, p.part, output.NewChainTimes(resp.Parts, loc), loc.String()))
	return nil
}

// givenInt is n when the flag was given, nil otherwise, for
// BuildCLICommand.
func givenInt(given bool, n int) *int {
	if !given {
		return nil
	}
	return &n
}

// readPartForCLI reads a part of the chain the way `rx samples` reads a
// file without building an index: with its stored index when one is
// current, from its text otherwise.
func readPartForCLI(ctx context.Context, _ logchain.Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
	return samples.Resolve(ctx, req)
}

// logsSamplesRequestOf checks the flags and builds the chain's samples
// request: exactly one of --lines and --timestamps, --part only with
// --lines, lines in the samples syntax, a zone that names one, and a
// well-formed fingerprint. A usage error exits 2.
func logsSamplesRequestOf(p logsSamplesParams) (logchain.SamplesRequest, config.Zone, error) {
	var req logchain.SamplesRequest
	switch {
	case (len(p.lines) > 0) == (len(p.timestamps) > 0):
		return req, config.Zone{}, exitWithError(os.Stderr, ExitUsageError, "must provide exactly one of --lines or --timestamps")
	case p.part != "" && len(p.lines) == 0:
		return req, config.Zone{}, exitWithError(os.Stderr, ExitUsageError, "--part= numbers the lines of one part: give it with --lines=")
	}
	if err := checkFingerprintFlag(p.fingerprint, 1); err != nil {
		return req, config.Zone{}, err
	}
	fileZone, err := parseFileTZ(p.fileTZ)
	if err != nil {
		return req, config.Zone{}, err
	}
	if len(p.lines) > 0 {
		parsed, parseErr := samples.ParseCSV(strings.Join(p.lines, ","))
		if parseErr != nil {
			return req, config.Zone{}, exitWithError(os.Stderr, ExitUsageError, "%s", parseErr.Error())
		}
		req.Lines = parsed
	}
	req.Part, req.Timestamps = p.part, p.timestamps
	req.BeforeContext, req.AfterContext = p.ctxLines, p.ctxLines
	if p.beforeSet {
		req.BeforeContext = p.beforeCtx
	}
	if p.afterSet {
		req.AfterContext = p.afterCtx
	}
	req.IndexLoader = samples.StoredIndex
	if config.GetBoolEnv("RX_NO_INDEX", false) {
		req.IndexLoader = samples.NoIndex
	}
	return req, fileZone, nil
}

// logsSamplesFailure prints why the lines could not be given and
// returns the exit code: 2 for a request at fault (a part that is not a
// member, a negative context, a time named wrongly), 6 for an invalid
// chain, 7 when a part changed after the chain was described, and the
// codes of `rx samples` otherwise.
func logsSamplesFailure(handle string, err error) *ExitError {
	switch {
	case errors.Is(err, logchain.ErrChainInvalid):
		return exitWithError(os.Stderr, ExitChainInvalid, "%s", err.Error())
	case errors.Is(err, logchain.ErrNotAPart), errors.Is(err, logchain.ErrSamplesRequest), samples.IsUsageError(err):
		return exitWithError(os.Stderr, ExitUsageError, "%s", err.Error())
	case errors.Is(err, paths.ErrFileChanged), errors.Is(err, fs.ErrNotExist):
		return exitWithError(os.Stderr, ExitChainChanged, "the files of the chain changed while they were read: %s", handle)
	}
	return exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
}

// warnAboutMissingChainPositions names on stderr every position the
// chain has no line for, as `rx samples` does for a file: the answer
// says -1 and null, and a person reading the terminal is told why. A
// line of a part (part) is named with the part. Before the chain is
// ready a target is never known, so a line is named only when the part
// has none of its window.
func warnAboutMissingChainPositions(w io.Writer, resp *rxtypes.ChainSamplesResponse, part string) {
	where := "the chain"
	if part != "" {
		where = output.Printable(part)
	}
	ready := resp.State == rxtypes.ChainStateReady
	keys := make([]string, 0, len(resp.Lines))
	for key, line := range resp.Lines {
		single := !strings.Contains(strings.TrimPrefix(key, "-"), "-")
		if single && ((ready && line == -1) || (!ready && resp.Samples[key] == nil)) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lineKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		_, _ = fmt.Fprintf(w, "Warning: line %s is not in %s.\n", key, where)
	}
	for _, query := range output.SortedTimeQueries(resp.Timestamps) {
		if resp.Timestamps[query] != -1 {
			continue
		}
		if strings.Contains(query, "..") {
			_, _ = fmt.Fprintf(w, "Warning: no line of the chain is in the range %s.\n", output.Printable(query))
			continue
		}
		_, _ = fmt.Fprintf(w, "Warning: no line at or after %s in the chain.\n", output.Printable(query))
	}
}

// lineKeyLess orders line keys by their number, as a person reads them.
func lineKeyLess(a, b string) bool {
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	if errA == nil && errB == nil && x != y {
		return x < y
	}
	return a < b
}
