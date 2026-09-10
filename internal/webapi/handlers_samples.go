package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// samplesInput is the query-string shape for GET /v1/samples.
//
// offsets and lines are mutually exclusive. Exactly one must be set;
// both-set or neither-set returns 400.
//
// context is an alias that sets both before_context and after_context.
// Individual contexts override it.
// Negative one sentinels for "not provided" are used here because huma
// forbids pointer query params. -1 is outside the normal value range
// (context must be >= 0), so it's a safe signal for "default".
type samplesInput struct {
	Path          string `query:"path" required:"true" example:"/var/log/app.log" doc:"File path to read from"`
	Offsets       string `query:"offsets" example:"100,200,300" doc:"Comma-separated byte offsets or ranges"`
	Lines         string `query:"lines" example:"100,200-205,-1" doc:"Comma-separated 1-based line numbers or ranges"`
	Context       int    `query:"context" minimum:"-1" default:"-1" example:"3" doc:"Context lines before AND after each offset (-1 = default 3)"`
	BeforeContext int    `query:"before_context" minimum:"-1" default:"-1" doc:"Context lines before each offset (-1 = default 3)"`
	AfterContext  int    `query:"after_context" minimum:"-1" default:"-1" doc:"Context lines after each offset (-1 = default 3)"`
}

// samplesOutput wraps the SamplesResponse body.
type samplesOutput struct {
	Body rxtypes.SamplesResponse
}

// nilIfNegative returns *int pointing to n when n>=0; returns nil when
// n is the -1 "not provided" sentinel.
func nilIfNegative(n int) *int {
	if n < 0 {
		return nil
	}
	return &n
}

// registerSamplesHandlers mounts GET /v1/samples.
//
// Matches rx-python/src/rx/web.py:1076-1535. The endpoint is the CLI's
// `rx samples` over HTTP: seek to offsets (or line numbers) in a file
// and emit ±context lines of surrounding text.
//
// Compressed files: byte offsets are rejected (400); only line mode is
// supported. This matches Python's behavior because byte offsets in a
// .gz file have no stable meaning after partial decompression.
func registerSamplesHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "samples",
		Method:      http.MethodGet,
		Path:        "/v1/samples",
		Summary:     "Get context lines around byte offsets or line numbers",
		Description: "Use this endpoint to view actual content around matches from /v1/trace.",
		Tags:        []string{"Context"},
		Responses:   sandboxResponses(api),
	}, func(_ context.Context, in *samplesInput) (out *samplesOutput, err error) {
		// One counter increment per request, whichever of the handler's
		// many returns is taken.
		defer func() { recordEndpoint(prometheus.RecordSamplesRequest, err) }()

		if s.cfg.RipgrepPath == "" {
			return nil, ErrServiceUnavailable("ripgrep is not available on this system")
		}

		// Sandbox.
		validated, err := paths.ValidatePathWithinRoots(in.Path)
		if err != nil {
			var perr *paths.ErrPathOutsideRoots
			if errors.As(err, &perr) {
				return nil, NewSandboxError(perr)
			}
			if !errors.Is(err, paths.ErrNoSearchRootsConfigured) {
				return nil, ErrForbidden(err.Error())
			}
			validated = in.Path
		}

		// Mutual exclusion on offsets/lines.
		if in.Offsets != "" && in.Lines != "" {
			return nil, ErrBadRequest("Cannot use both 'offsets' and 'lines'. Provide only one.")
		}
		if in.Offsets == "" && in.Lines == "" {
			return nil, ErrBadRequest("Must provide either 'offsets' or 'lines' parameter.")
		}

		// Compression check.
		compressed := compression.IsCompressed(validated)
		compFmt := compression.FormatNone
		if compressed {
			compFmt, _ = compression.DetectFromPath(validated)
		}
		if compressed && in.Offsets != "" {
			return nil, ErrBadRequest(
				"Byte offsets are not supported for compressed files. Use 'lines' parameter instead.",
			)
		}
		_ = compFmt

		// Context defaults (-1 sentinel = "not provided").
		defaultCtx := 3
		before := defaultCtx
		after := defaultCtx
		if in.Context >= 0 {
			before = in.Context
			after = in.Context
		}
		if in.BeforeContext >= 0 {
			before = in.BeforeContext
		}
		if in.AfterContext >= 0 {
			after = in.AfterContext
		}

		// Parse the offsets/lines spec via the shared parser. Stage 9
		// Round 2 U rework: both CLI and HTTP delegate to
		// internal/samples.ParseCSV so the range syntax
		// (100,200-300,-5) stays identical across entry points.
		var (
			parsedOffsets []samples.OffsetOrRange
			parsedLines   []samples.OffsetOrRange
		)
		if in.Offsets != "" {
			parsedOffsets, err = samples.ParseCSV(in.Offsets)
			if err != nil {
				return nil, ErrBadRequest(fmt.Sprintf("Invalid offsets format: %s", err.Error()))
			}
		} else {
			parsedLines, err = samples.ParseCSV(in.Lines)
			if err != nil {
				return nil, ErrBadRequest(fmt.Sprintf("Invalid lines format: %s", err.Error()))
			}
		}

		// File existence / type check.
		stat, err := os.Stat(validated)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNotFound(fmt.Sprintf("File not found: %s", validated))
			}
			return nil, ErrForbidden(err.Error())
		}
		if stat.IsDir() {
			return nil, ErrBadRequest(fmt.Sprintf("Path is a directory, not a file: %s", validated))
		}

		// One resolver for both file kinds and both entry points: it
		// reads a plain file by offset or by line, and streams a
		// compressed one through its decompressor.
		loader := func(path string) (*rxtypes.UnifiedFileIndex, error) {
			idx, loadErr := index.LoadForSource(path)
			if loadErr != nil {
				if errors.Is(loadErr, index.ErrIndexNotFound) {
					return nil, nil
				}
				return nil, loadErr
			}
			return idx, nil
		}
		resp, err := samples.Resolve(samples.Request{
			Path:          validated,
			Offsets:       parsedOffsets,
			Lines:         parsedLines,
			BeforeContext: before,
			AfterContext:  after,
			IndexLoader:   loader,
		})
		if err != nil {
			if errors.Is(err, samples.ErrOffsetsOnCompressed) {
				return nil, ErrBadRequest(err.Error())
			}
			return nil, ErrInternal(err.Error())
		}

		// cli_command equivalent. Use resolved values (after defaults).
		// CLICommand is *string per Stage 9 Round 2 S2 rule (null vs
		// empty string distinction); &cli wraps the builder output.
		cli := BuildCLICommand("samples", map[string]any{
			"path":           validated,
			"offsets":        in.Offsets,
			"lines":          in.Lines,
			"context":        nilIfNegative(in.Context),
			"before_context": nilIfNegative(in.BeforeContext),
			"after_context":  nilIfNegative(in.AfterContext),
		})
		resp.CLICommand = &cli

		return &samplesOutput{Body: *resp}, nil
	})
}
