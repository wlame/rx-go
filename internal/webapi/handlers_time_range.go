package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// timeRangeInput is the query string of GET /v1/time-range.
type timeRangeInput struct {
	Path   string `query:"path" required:"true" example:"/var/log/app.log" doc:"The file whose time range to give"`
	FileTZ string `query:"file_tz" example:"Europe/Berlin" doc:"Read the file's timestamps as the wall clock each line writes, in this zone: UTC, an IANA zone name or ±HH:MM (as RX_LOG_TZ takes it). A zone a line writes is ignored, and the zone takes the place of RX_LOG_TZ for this request. Empty or absent: the file is read as its timestamps say. Another value is refused with 400."`
}

// fileZoneOf reads the file_tz query parameter of GET /v1/samples and
// GET /v1/time-range: no zone when it is empty, and a 400 naming the
// value when it names none (config.ParseZone).
func fileZoneOf(value string) (config.Zone, error) {
	if value == "" {
		return config.Zone{}, nil
	}
	zone, err := config.ParseZone(value)
	if err != nil {
		return config.Zone{}, ErrBadRequest(fmt.Sprintf("Invalid file_tz %q: %s", value, err.Error()))
	}
	return zone, nil
}

// timeRangeOutput wraps the answer for huma, which takes the body from
// the Body field.
type timeRangeOutput struct {
	Body rxtypes.TimeRangeResponse
}

// registerTimeRangeHandlers mounts GET /v1/time-range: the time range
// of one file, which `rx time-range` gives from a terminal.
//
// The request reads at most the head of the file's text and 16 MiB
// back from its end, or nothing when an index holds the range. For a
// seekable zstd file the frames it decodes for the read back hold at
// most samples.TimeRangeDecodeBytes of text, since a frame decodes
// whole. It
// never builds an index and never decompresses a whole file: a gzip,
// bzip2, xz or plain zstd file without an index answers source "none",
// and a client asks again once the index a samples request starts has
// been built.
func registerTimeRangeHandlers(_ *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "time_range",
		Method:      http.MethodGet,
		Path:        "/v1/time-range",
		Summary:     "Get the timestamp format and the first and last timestamp of a file",
		Description: "The time range of one file for a timeline: its format, its first and last timestamp " +
			"as UTC instants, the zone its lines show times in and its first timestamp as written. " +
			"From the file's line index when there is one; otherwise from the head of its text and at most " +
			"16 MiB back from its end, or not at all for a stream-compressed file (source none). " +
			"file_tz reads the timestamps as wall clock in a chosen zone; for a file whose timestamps carry zones " +
			"the index gives each timestamp with the offset its line writes. When the offset changes too often for " +
			"the index to record, the index gives the first timestamp and the last timestamped line is read again " +
			"at the offset it stores (source none for a stream-compressed file).",
		Tags:      []string{"Context"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *timeRangeInput) (*timeRangeOutput, error) {
		validated, err := paths.ValidatePathWithinRoots(in.Path)
		if err != nil {
			var outside *paths.ErrPathOutsideRoots
			if errors.As(err, &outside) {
				return nil, NewSandboxError(outside)
			}
			if !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
				return nil, ErrForbidden(err.Error())
			}
			validated = in.Path
		}
		fileZone, err := fileZoneOf(in.FileTZ)
		if err != nil {
			return nil, err
		}

		// SECURITY: the file is read only through this pin, which also
		// answers whether it exists: no stat of the path before it.
		source, err := paths.Pin(validated)
		if err != nil {
			return nil, ErrFileAccess(validated, err)
		}
		if source.Info().IsDir() {
			return nil, ErrBadRequest(fmt.Sprintf("Path is a directory, not a file: %s", validated))
		}
		kind, err := samples.Classify(samples.Request{Source: source})
		if errors.Is(err, filekind.ErrNotText) {
			return nil, ErrBadRequest(fmt.Sprintf("%s: %s", err.Error(), validated))
		}
		if err != nil {
			return nil, ErrFileAccess(validated, err)
		}

		// RX_NO_INDEX reads no index file, as it does for samples.
		loader := samples.StoredIndex
		if config.GetBoolEnv("RX_NO_INDEX", false) {
			loader = samples.NoIndex
		}
		// ctx ends when the client disconnects, which stops the read.
		resp, err := samples.TimeRange(ctx, samples.Request{
			Path: validated, Source: source, Kind: &kind, IndexLoader: loader, FileZone: fileZone,
		})
		if errors.Is(err, compression.ErrTooLargeToDecode) {
			return nil, ErrBadRequest(fmt.Sprintf("%s: %s", compression.TooLargeToDecodeReason, validated))
		}
		if err != nil {
			return nil, ErrInternal(err.Error())
		}
		resp.CLICommand = BuildCLICommand("time_range", map[string]any{"path": validated, "file_tz": in.FileTZ})
		return &timeRangeOutput{Body: *resp}, nil
	})
}
