package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// samplesInput is the query-string shape for GET /v1/samples.
//
// offsets, lines and timestamps are mutually exclusive. Exactly one
// must be set; more than one, or none, returns 400.
//
// timestamps repeats (`?timestamps=A&timestamps=B`) and is never split
// at commas: a comma is part of some timestamp formats
// (`12:34:56,123`), which is why it is a list where offsets and lines
// are comma-separated strings.
//
// context is an alias that sets both before_context and after_context.
// Individual contexts override it. Each stops at MaxTraceContextLines,
// the trace endpoint's cap; the tags spell the number.
// Negative one sentinels for "not provided" are used here because huma
// forbids pointer query params. -1 is outside the normal value range
// (context must be >= 0), so it's a safe signal for "default".
type samplesInput struct {
	Path          string   `query:"path" required:"true" example:"/var/log/app.log" doc:"File path to read from"`
	Offsets       string   `query:"offsets" example:"100,200,300" doc:"Comma-separated byte offsets or ranges"`
	Lines         string   `query:"lines" example:"100,200-205,-1" doc:"Comma-separated 1-based line numbers or ranges"`
	Timestamps    []string `query:"timestamps,explode" example:"2025-12-10T07:30:00" doc:"A time or time range (T, T1..T2, ..T2, T1..); repeat the parameter for several, at most 1000. Each answers the first line whose own timestamp is at or after the time, with context, or a range's lines without context. A value is never split at commas."`
	Context       int      `query:"context" minimum:"-1" maximum:"100" default:"-1" example:"3" doc:"Context lines before AND after each offset (-1 = default 3)"`
	BeforeContext int      `query:"before_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines before each offset (-1 = default 3)"`
	AfterContext  int      `query:"after_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines after each offset (-1 = default 3)"`
	Prefer        string   `header:"Prefer" doc:"RFC 7240 preferences. respond-async lets the server answer 202 with the task building the file's line index when the build outlasts RX_SAMPLES_WAIT_SECONDS; without it the request waits for the build and answers 200."`
}

// samplesOutput is the answer of GET /v1/samples: 200 with an
// rxtypes.SamplesResponse, or 202 with an rxtypes.TaskResponse naming the
// index build the lookup is waiting for.
//
// huma reads the status from the Status field when the output struct
// has one. Body is `any` because the two statuses carry different
// bodies; the OpenAPI document gets the schema of each from the
// operation's declared responses (samplesResponses), which huma keeps
// rather than deriving one from this type.
//
// PreferenceApplied is set on the 202 only. It is hidden from huma's
// generated document, which would declare it on the 200;
// samplesResponses declares it on the 202 by hand.
type samplesOutput struct {
	Status            int
	PreferenceApplied string `header:"Preference-Applied" hidden:"true"`
	Body              any
}

// samplesResponses declares the two success answers of GET /v1/samples
// beside its error answers.
func samplesResponses(api huma.API) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden,
		http.StatusNotFound, http.StatusUnprocessableEntity,
		http.StatusInternalServerError)
	responses[strconv.Itoa(http.StatusOK)] = jsonResponse("OK",
		registry.Schema(reflect.TypeOf(rxtypes.SamplesResponse{}), true, "SamplesResponse"))
	accepted := jsonResponse(
		"Sent only to a request with `Prefer: respond-async`: the file's line index is being built "+
			"and did not finish within the server's wait (RX_SAMPLES_WAIT_SECONDS). The body names the "+
			"build's task: poll GET /v1/tasks/{task_id} until it ends, then send the same request again.",
		registry.Schema(reflect.TypeOf(rxtypes.TaskResponse{}), true, "TaskResponse"))
	accepted.Headers = map[string]*huma.Param{
		"Preference-Applied": {
			Description: "respond-async: the server applied the preference the request sent (RFC 7240).",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
	responses[strconv.Itoa(http.StatusAccepted)] = accepted
	return responses
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
// Compressed files answer both modes. A byte offset is a position in
// the file's text, the decompressed stream, which is the coordinate a
// trace of the same file reports its matches in.
func registerSamplesHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "samples",
		Method:      http.MethodGet,
		Path:        "/v1/samples",
		Summary:     "Get context lines around byte offsets, line numbers or times",
		Description: "Use this endpoint to view actual content around matches from /v1/trace.",
		Tags:        []string{"Context"},
		// No 503: samples reads the file itself and never runs ripgrep.
		Responses: samplesResponses(api),
	}, func(ctx context.Context, in *samplesInput) (out *samplesOutput, err error) {
		start := time.Now()
		// One counter increment per request, whichever of the handler's
		// many returns is taken.
		defer func() { recordEndpoint(prometheus.RecordSamplesRequest, err) }()

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

		// Mutual exclusion on offsets/lines/timestamps.
		if in.Offsets != "" && in.Lines != "" {
			return nil, ErrBadRequest("Cannot use both 'offsets' and 'lines'. Provide only one.")
		}
		if len(in.Timestamps) > 0 && (in.Offsets != "" || in.Lines != "") {
			return nil, ErrBadRequest("Cannot use 'timestamps' with 'offsets' or 'lines'. Provide only one.")
		}
		if in.Offsets == "" && in.Lines == "" && len(in.Timestamps) == 0 {
			return nil, ErrBadRequest("Must provide one of 'offsets', 'lines' or 'timestamps'.")
		}

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

		// Parse the offsets/lines spec via the shared parser: both CLI
		// and HTTP delegate to internal/samples.ParseCSV so the range
		// syntax (100,200-300,-5) stays identical across entry points.
		var (
			parsedOffsets []samples.OffsetOrRange
			parsedLines   []samples.OffsetOrRange
		)
		switch {
		case in.Offsets != "":
			parsedOffsets, err = samples.ParseCSV(in.Offsets)
			if err != nil {
				return nil, ErrBadRequest(fmt.Sprintf("Invalid offsets format: %s", err.Error()))
			}
		case in.Lines != "":
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

		// A second lookup in a multi-gigabyte file is the case an index
		// exists for, so one is built when the file is worth it and none
		// is cached — the same rule `rx samples` follows, so the two
		// surfaces leave the same state on disk. RX_NO_INDEX opts out;
		// there is no query parameter for it, because the decision
		// belongs to whoever runs the server rather than to a caller.
		//
		// The build runs as a background task shared by every request
		// for the file. A request that prefers respond-async waits for
		// it up to the server's wait and answers 202 with the task when
		// the build takes longer, so the first look at a 50 GB file does
		// not hold its HTTP request open while all of it is read. Any
		// other request waits for the build and answers the lines.
		//
		// When the cache cannot store the index, no task is started:
		// it would read the whole file for an index nobody keeps, once
		// per request. The request reads the file without an index,
		// and the server logs the cause once (samples.ShouldBuildIndex).
		// RX_NO_INDEX is checked first, so it never touches the cache.
		// What the file is, decided once through its pin, before any
		// index is built for it: a file whose text is not text has no
		// lines, so it is refused rather than answered with its bytes.
		source, err := paths.Pin(validated)
		if err != nil {
			return nil, ErrFileAccess(validated, err)
		}
		kind, err := samples.Classify(samples.Request{Source: source})
		if errors.Is(err, filekind.ErrNotText) {
			return nil, ErrBadRequest(fmt.Sprintf("%s: %s", err.Error(), validated))
		}
		if err != nil {
			// A file the server may not read is a 403 on every route.
			return nil, ErrFileAccess(validated, err)
		}

		noIndex := config.GetBoolEnv("RX_NO_INDEX", false)
		if !noIndex && samples.ShouldBuildIndex(validated, kind, stat.Size()) {
			deadline, stopDeadline := samplesDeadline(in.Prefer, s.cfg.SamplesIndexWait)
			pending, waitErr := s.samplesIndex.await(ctx, validated, stat, deadline)
			stopDeadline()
			if waitErr != nil {
				return nil, waitErr
			}
			if pending != nil {
				return &samplesOutput{
					Status: http.StatusAccepted, PreferenceApplied: respondAsync, Body: *pending,
				}, nil
			}
		}

		// One resolver for both file kinds and both entry points: it
		// reads a plain file by offset or by line, and streams a
		// compressed one through its decompressor. A missing, stale or
		// damaged index is an absent one, never a 500; under
		// RX_NO_INDEX no index file is read at all, as `rx samples
		// --no-index` reads none.
		loader := samples.StoredIndex
		if noIndex {
			loader = samples.NoIndex
		}
		resp, err := samples.Resolve(samples.Request{
			Path:          validated,
			Source:        source,
			Kind:          &kind,
			Offsets:       parsedOffsets,
			Lines:         parsedLines,
			Timestamps:    in.Timestamps,
			BeforeContext: before,
			AfterContext:  after,
			IndexLoader:   loader,
		})
		if samples.IsUsageError(err) {
			// A time the request names wrongly, or a time query on a
			// file without timestamps: the request is at fault.
			return nil, ErrBadRequest(err.Error())
		}
		if err != nil {
			return nil, ErrInternal(err.Error())
		}

		// cli_command equivalent. Use resolved values (after defaults).
		// CLICommand is *string per (null vs
		// empty string distinction); &cli wraps the builder output.
		cli := BuildCLICommand("samples", map[string]any{
			"path":           validated,
			"offsets":        in.Offsets,
			"lines":          in.Lines,
			"timestamps":     in.Timestamps,
			"context":        nilIfNegative(in.Context),
			"before_context": nilIfNegative(in.BeforeContext),
			"after_context":  nilIfNegative(in.AfterContext),
		})
		resp.CLICommand = &cli

		observeSamplesResult(start, len(parsedOffsets)+len(parsedLines)+len(in.Timestamps), before, after)
		return &samplesOutput{Status: http.StatusOK, Body: *resp}, nil
	})
}
