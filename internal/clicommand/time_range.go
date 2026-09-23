package clicommand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/webapi"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// NewTimeRangeCommand builds the `rx time-range` cobra command: the
// time range of each file, as GET /v1/time-range gives it.
//
//	rx time-range app.log                 # one line per file
//	rx time-range app.log db.log --json   # an array, one object per file
//	rx time-range app.log --file-tz=Asia/Tokyo  # its wall clock read in Tokyo
//
// A gzip, bzip2, xz or plain zstd file without a line index gets one
// built first, as `rx samples` builds one, so its range comes from the
// index; RX_NO_INDEX turns that off, and such a file then answers
// source "none".
func NewTimeRangeCommand(out io.Writer) *cobra.Command {
	var jsonOutput bool
	var fileTZ string
	cmd := &cobra.Command{
		Use:   "time-range PATH...",
		Short: "Show the format and the first and last timestamp of each file",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			fileZone, err := parseFileTZ(fileTZ)
			if err != nil {
				return err
			}
			return runTimeRange(out, args, jsonOutput, fileZone)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false,
		"Output JSON: one object for one path, an array of objects for several")
	cmd.Flags().StringVar(&fileTZ, "file-tz", "", fileTZFlagUsage)
	return cmd
}

// runTimeRange answers every path in order. A path it cannot answer is
// reported on stderr and the others are still answered; the exit code
// is then that path's (multiPathFailure), as `rx index` does for
// several paths. fileZone, when it names a zone, reads every file in it
// (samples.Request.FileZone).
func runTimeRange(out io.Writer, pathArgs []string, jsonOutput bool, fileZone config.Zone) error {
	answers := make([]*rxtypes.TimeRangeResponse, 0, len(pathArgs))
	var failureCodes []int
	var single *ExitError
	for _, path := range pathArgs {
		resp, failure := timeRangeOf(path, fileZone)
		if failure != nil {
			failureCodes = append(failureCodes, failure.Code)
			single = failure
			continue
		}
		answers = append(answers, resp)
	}
	if err := writeTimeRanges(out, answers, len(pathArgs) == 1, jsonOutput, fileZone); err != nil {
		return err
	}
	if len(pathArgs) == 1 && single != nil {
		// One path: its own message and exit code, as `rx samples`.
		return single
	}
	return multiPathFailure(failureCodes, "one or more files have no time range")
}

// writeTimeRanges prints the answers: as JSON, one object when one
// path was given and an array otherwise; or one line per file, with the
// times shown in fileZone when it names one.
func writeTimeRanges(out io.Writer, answers []*rxtypes.TimeRangeResponse, onePath, jsonOutput bool, fileZone config.Zone) error {
	if !jsonOutput {
		for _, resp := range answers {
			_, _ = fmt.Fprintln(out, timeRangeLine(resp, fileZone))
		}
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if onePath {
		if len(answers) == 0 {
			return nil
		}
		return enc.Encode(answers[0])
	}
	return enc.Encode(answers)
}

// timeRangeOf answers one path, or returns the error the command exits
// with for it (already printed on stderr).
func timeRangeOf(path string, fileZone config.Zone) (*rxtypes.TimeRangeResponse, *ExitError) {
	if _, err := sandboxCheck(path); err != nil {
		return nil, exitWithError(os.Stderr, ExitAccessDenied, "%s", err.Error())
	}
	source, err := paths.Pin(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, exitWithError(os.Stderr, ExitFileNotFound, "file not found: %s", path)
	}
	if err != nil {
		return nil, openFailure(path, err)
	}
	if source.Info().IsDir() {
		return nil, exitWithError(os.Stderr, ExitUsageError, "path is a directory, not a file: %s", path)
	}
	kind, err := samples.Classify(samples.Request{Source: source})
	if errors.Is(err, filekind.ErrNotText) {
		return nil, exitWithError(os.Stderr, ExitUsageError, "%s: %s", err.Error(), path)
	}
	if err != nil {
		return nil, openFailure(path, err)
	}

	noIndex := config.GetBoolEnv("RX_NO_INDEX", false)
	buildStreamIndexIfMissing(path, kind, source.Info().Size(), noIndex)
	loader := samples.StoredIndex
	if noIndex {
		loader = samples.NoIndex
	}
	resp, err := samples.TimeRange(context.Background(), samples.Request{
		Path: path, Source: source, Kind: &kind, IndexLoader: loader, FileZone: fileZone,
	})
	if errors.Is(err, compression.ErrTooLargeToDecode) {
		return nil, exitWithError(os.Stderr, ExitGenericError, "%s: %s", compression.TooLargeToDecodeReason, path)
	}
	if err != nil {
		return nil, exitWithError(os.Stderr, ExitGenericError, "%s", err.Error())
	}
	resp.CLICommand = webapi.BuildCLICommand("time_range", map[string]any{"path": path, "file_tz": fileZone.Name})
	return resp, nil
}

// buildStreamIndexIfMissing builds and stores the line index of a gzip,
// bzip2, xz or plain zstd file that has none, the one kind of file
// whose time range needs it: its last timestamp is at the end of a text
// that can only be read from the start. A plain or seekable file is
// answered from its head and tail without one, however large. A build
// that fails is logged, and the file answers source "none".
func buildStreamIndexIfMissing(path string, kind filekind.Kind, size int64, noIndex bool) {
	if noIndex || !kind.IsCompressed() || kind.IsSeekable() {
		return
	}
	if !samples.ShouldBuildIndex(path, kind, size) {
		return
	}
	if _, _, err := samples.BuildIndex(path, nil); err != nil {
		slog.Default().Warn("index_not_built", "path", path, "error", err.Error())
	}
}

// unknownTime stands for a first or last timestamp the answer does not
// know.
const unknownTime = "?"

// timeRangeLine is the human line of one answer: the path, the format,
// the first and last timestamp in the file's own layout and zone, the
// zone, and where the range came from. fileZone is the request's file
// zone, which the times are shown in when it names one.
//
//	app.log  iso  2025-12-10 07:00:04.574 .. 2025-12-10 07:59:59.390  UTC  index
//	notes.txt  no timestamps  scan
func timeRangeLine(resp *rxtypes.TimeRangeResponse, fileZone config.Zone) string {
	path := output.Printable(resp.Path)
	if resp.Format == nil {
		return strings.Join([]string{path, "no timestamps", resp.Source}, "  ")
	}
	zone := unknownTime
	if resp.DisplayZone != nil {
		zone = *resp.DisplayZone
	}
	style := output.FileTimeStyle{Family: *resp.Format, DayFirst: resp.DayFirst != nil && *resp.DayFirst}
	if resp.Example != nil {
		style.Example = *resp.Example
	}
	loc := displayLocation(resp, fileZone)
	span := fileTimeOrUnknown(resp.FirstMs, style, loc) + " .. " + fileTimeOrUnknown(resp.LastMs, style, loc)
	return strings.Join([]string{path, *resp.Format, span, output.Printable(zone), resp.Source}, "  ")
}

// fileTimeOrUnknown renders an instant in the file's layout, or "?".
func fileTimeOrUnknown(ms *int64, style output.FileTimeStyle, loc *time.Location) string {
	if ms == nil {
		return unknownTime
	}
	return output.FileTime(*ms, style, loc)
}

// displayLocation is the zone an answer's times are shown in: the file
// zone when the request named one (display_zone names it too), the
// offset of display_zone for a file whose timestamps carry zones, and
// RX_LOG_TZ, which display_zone names, for one whose timestamps do not.
func displayLocation(resp *rxtypes.TimeRangeResponse, fileZone config.Zone) *time.Location {
	if fileZone.Location != nil {
		return fileZone.Location
	}
	if resp.HasZone != nil && *resp.HasZone {
		if resp.DisplayZone != nil {
			if loc, ok := output.FixedOffsetZone(*resp.DisplayZone); ok {
				return loc
			}
		}
		return time.UTC
	}
	if loc := config.LogTZ().Location; loc != nil {
		return loc
	}
	return time.UTC
}
