package webapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logSamplesInput is the query string of GET /v1/logs/samples.
//
// lines and timestamps are exclusive: exactly one is given. part goes
// with lines only. The context parameters take the -1 sentinel for "not
// given", as on GET /v1/samples, since huma forbids pointer query
// parameters.
type logSamplesInput struct {
	Path          string   `query:"path" required:"true" example:"/var/log/syslog" doc:"The chain's handle: its directory joined with its name, as GET /v1/logs/chains gives it in path."`
	Lines         string   `query:"lines" example:"123456,100-200,-1" doc:"Comma-separated 1-based line numbers or ranges: the chain's global numbers, or with part the part's own numbers. -N counts back from the end (of the chain, or of the part)."`
	Part          string   `query:"part" example:"syslog.3.gz" doc:"The bare name of a part, as GET /v1/logs/chain lists it: lines then number that part as GET /v1/samples numbers it on its own. Only with lines. Before the chain is ready, this is the only way to read it: the part is read alone."`
	Timestamps    []string `query:"timestamps,explode" example:"2026-10-03T14:00:00" doc:"A time or time range (T, T1..T2, ..T2, T1..), as GET /v1/samples takes it; repeat the parameter for several, at most 1000. Each answers the first line, in the chain's order, whose own timestamp is at or after the time, with context, or a range's lines without context. A time of day without a date takes its date from the chain's first and last timestamps, which must fall on one day."`
	Context       int      `query:"context" minimum:"-1" maximum:"100" default:"-1" example:"3" doc:"Context lines before AND after each single line or time (-1 = default 3)"`
	BeforeContext int      `query:"before_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines before each single line or time (-1 = default 3)"`
	AfterContext  int      `query:"after_context" minimum:"-1" maximum:"100" default:"-1" doc:"Context lines after each single line or time (-1 = default 3)"`
	FileTZ        string   `query:"file_tz" example:"Europe/Berlin" doc:"Read every part's timestamps as the wall clock each line writes, in this zone: UTC, an IANA zone name or ±HH:MM, as GET /v1/samples reads one file. Another value is refused with 400."`
	Fingerprint   string   `query:"fingerprint" pattern:"^[0-9a-f]{16}$" example:"3fa2c4d5e6f70812" doc:"The fingerprint of a description the client holds. When the chain's files changed since (the fingerprint differs), the answer is 409 with the current description."`
	Prefer        string   `header:"Prefer" doc:"RFC 7240 preferences. respond-async lets the server answer 202 with the task it waits for (the chain's index task, or a part's index build) once RX_SAMPLES_WAIT_SECONDS has passed; without it the request waits for the task and answers 200."`
}

// logSamplesOutput is the answer of GET /v1/logs/samples: 200 with an
// rxtypes.ChainSamplesResponse, 202 with the rxtypes.TaskResponse of the
// task the request waits for, or 409 with the chain's current
// rxtypes.ChainResponse. huma reads the status from Status; Body is
// `any` because the three carry different bodies, whose schemas come
// from the declared responses (logSamplesResponses).
type logSamplesOutput struct {
	Status            int
	PreferenceApplied string `header:"Preference-Applied" hidden:"true"`
	Body              any
}

// logSamplesResponses declares the answers of GET /v1/logs/samples
// beside its error answers.
func logSamplesResponses(api huma.API) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusInternalServerError)
	responses[strconv.Itoa(http.StatusOK)] = jsonResponse("The lines asked for, each key's as pieces, one per part "+
		"its window touches.", registry.Schema(reflect.TypeOf(rxtypes.ChainSamplesResponse{}), true, "ChainSamplesResponse"))
	accepted := jsonResponse("Sent only to a request with `Prefer: respond-async`: the answer waits for a task that did "+
		"not end within the server's wait (RX_SAMPLES_WAIT_SECONDS): the chain's index task (operation chain_index) "+
		"for a request by global line or by time on a pending chain, or the line index build of the part a piece "+
		"lies in. Poll GET /v1/tasks/{task_id} until it ends, then send the same request again.",
		registry.Schema(reflect.TypeOf(rxtypes.TaskResponse{}), true, "TaskResponse"))
	accepted.Headers = map[string]*huma.Param{
		"Preference-Applied": {
			Description: "respond-async: the server applied the preference the request sent (RFC 7240).",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
	responses[strconv.Itoa(http.StatusAccepted)] = accepted
	responses[strconv.Itoa(http.StatusConflict)] = jsonResponse("The chain's files changed: the fingerprint the "+
		"request sent differs from the current one, or a part was renamed or replaced while the request read it. "+
		"The body is the current description.",
		registry.Schema(reflect.TypeOf(rxtypes.ChainResponse{}), true, "ChainResponse"))
	responses[strconv.Itoa(http.StatusServiceUnavailable)] = chainTasksFullResponse(api,
		"A request by global line or by time on a pending chain whose index task does not run and cannot start "+
			"now: as many log chain index tasks as the server runs at once are running or waiting. Ask again once "+
			"one of them has ended.")
	return responses
}

// logSamplesRequest is a GET /v1/logs/samples request after its
// parameters were checked: the chain's samples request, the file zone,
// and the context values as given, for the answer's cli_command.
type logSamplesRequest struct {
	samples  logchain.SamplesRequest
	fileZone config.Zone
}

// parseLogSamplesInput checks the parameters of a chain samples request
// and builds it, before anything is read: exactly one of lines and
// timestamps, part only with lines and only as a bare name, lines in
// the samples syntax, a zone that names one. The errors are 400s.
//
// SECURITY: part is compared with the chain's part names only after
// this, and never joined to a path; a value with a separator, "." or
// ".." is refused here, so no value reaches the filesystem.
func parseLogSamplesInput(in *logSamplesInput) (logSamplesRequest, error) {
	var out logSamplesRequest
	switch {
	case in.Lines != "" && len(in.Timestamps) > 0:
		return out, ErrBadRequest("Cannot use both 'lines' and 'timestamps'. Provide only one.")
	case in.Lines == "" && len(in.Timestamps) == 0:
		return out, ErrBadRequest("Must provide one of 'lines' or 'timestamps'.")
	case in.Part != "" && in.Lines == "":
		return out, ErrBadRequest("'part' numbers the lines of one part: give it with 'lines'.")
	case in.Part != "" && !isBareName(in.Part):
		return out, ErrBadRequest(fmt.Sprintf("'part' must be the bare name of a part, as GET /v1/logs/chain lists it: %q", in.Part))
	}
	zone, err := fileZoneOf(in.FileTZ)
	if err != nil {
		return out, err
	}
	out.fileZone = zone
	if in.Lines != "" {
		parsed, err := samples.ParseCSV(in.Lines)
		if err != nil {
			return out, ErrBadRequest(fmt.Sprintf("Invalid lines format: %s", err.Error()))
		}
		out.samples.Lines = parsed
	}
	out.samples.Part = in.Part
	out.samples.Timestamps = in.Timestamps
	out.samples.BeforeContext, out.samples.AfterContext = contextOf(in.Context, in.BeforeContext, in.AfterContext)
	return out, nil
}

// isBareName reports whether name can be a file name in a directory:
// not empty, without a path separator, and neither "." nor "..".
func isBareName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

// contextOf resolves the context parameters as GET /v1/samples does:
// 3 lines each by default, context sets both, before_context and
// after_context each override it (-1: not given).
func contextOf(both, before, after int) (int, int) {
	const defaultContext = 3
	b, a := defaultContext, defaultContext
	if both >= 0 {
		b, a = both, both
	}
	if before >= 0 {
		b = before
	}
	if after >= 0 {
		a = after
	}
	return b, a
}

// registerLogSamplesHandler mounts GET /v1/logs/samples: lines of a log
// chain by global line, by a part and its own line, or by time, which
// `rx logs samples` gives from a terminal.
//
// The request describes the chain as GET /v1/logs/chain does, then
// reads each part a window touches through the path GET /v1/samples
// reads one file by (readFileSamples): a part's stored index, an answer
// from the head of a part that wants an index and has none, and the
// wait for its build. A request by global line or by time on a pending
// chain first waits for the chain's index task (awaitChainReady).
func registerLogSamplesHandler(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "log_samples",
		Method:      http.MethodGet,
		Path:        "/v1/logs/samples",
		Summary:     "Get lines of a log chain by global line, by part and line, or by time",
		Description: "Lines of one log chain, as GET /v1/samples gives them for one file: by the chain's global " +
			"line numbers (lines), by a part and its own numbers (part and lines), or by time (timestamps). Each key's " +
			"lines come as pieces, one per part its window touches, with the part's name, its first local and global " +
			"line, the lines and their line_timestamps, part_start and part_end, and the rx samples command for exactly " +
			"that piece. Context crosses part edges once the chain is ready; before that only part and lines are " +
			"answered, reading that part alone, and a global or time request waits up to RX_SAMPLES_WAIT_SECONDS for " +
			"the chain's index task (202 with the task under Prefer: respond-async). RX_SAMPLES_MAX_LINES and " +
			"RX_SAMPLES_MAX_BYTES bound the whole answer. 409 with the current description when fingerprint differs " +
			"or a part changed while it was read; 422 for an invalid chain; 404 when the handle names fewer than two parts.",
		Tags:      []string{"Logs"},
		Responses: logSamplesResponses(api),
	}, func(ctx context.Context, in *logSamplesInput) (*logSamplesOutput, error) {
		parsed, err := parseLogSamplesInput(in)
		if err != nil {
			return nil, err
		}
		opts := logchain.Options{FileZone: parsed.fileZone}
		// ctx ends when the client disconnects, which stops every read
		// and every wait.
		d, changed, err := logchain.DescribeHandle(ctx, in.Path, opts)
		if err != nil {
			return nil, logChainError(in.Path, err)
		}
		if logChainStatus(changed, in.Fingerprint, d.Response.Fingerprint) != http.StatusOK {
			return s.chainConflict(d, in), nil
		}
		limit := newWaitLimit(in.Prefer, s.cfg.SamplesIndexWait)
		if parsed.samples.Part == "" && d.Response.State == rxtypes.ChainStatePending {
			var waited *logSamplesOutput
			if d, waited, err = s.awaitChainReady(ctx, in, opts, d, limit); err != nil || waited != nil {
				return waited, err
			}
		}
		chainBuild, _ := s.chainIndex.forDescription(d)
		reader := s.chainPartReader(limit)
		parsed.samples.MaxLines, parsed.samples.MaxBytes = config.SamplesMaxLines(), config.SamplesMaxBytes()
		parsed.samples.IndexLoader = samples.StoredIndex
		if reader.noIndex {
			parsed.samples.IndexLoader = samples.NoIndex
		}
		resp, err := logchain.Samples(ctx, d, parsed.samples, reader.read)
		if err != nil {
			return s.logSamplesError(ctx, in, opts, err)
		}
		resp.IndexBuild = reader.built
		if d.Response.State == rxtypes.ChainStatePending {
			resp.IndexBuild = chainBuild
		}
		FillChainSamplesCommands(resp, in.FileTZ, map[string]any{
			"path": resp.Path, "lines": in.Lines, "part": in.Part, "timestamps": in.Timestamps, "file_tz": in.FileTZ,
			"fingerprint": in.Fingerprint, "context": nilIfNegative(in.Context),
			"before_context": nilIfNegative(in.BeforeContext), "after_context": nilIfNegative(in.AfterContext),
		})
		return &logSamplesOutput{Status: http.StatusOK, Body: *resp}, nil
	})
}

// FillChainSamplesCommands writes the cli_command of a chain samples
// answer, from params (the request's values, as BuildCLICommand takes
// them for `rx logs samples`), and of each of its pieces: the
// `rx samples` command that gives exactly the piece's lines from its
// part, a range of the part's own numbers (a single line too, as A-A,
// which takes no context), read in fileTZ when it names a zone.
func FillChainSamplesCommands(resp *rxtypes.ChainSamplesResponse, fileTZ string, params map[string]any) {
	resp.CLICommand = BuildCLICommand("logs_samples", params)
	paths := make(map[string]string, len(resp.Parts))
	for _, p := range resp.Parts {
		paths[p.Name] = p.Path
	}
	for _, pieces := range resp.Samples {
		for i := range pieces {
			p := &pieces[i]
			last := p.FirstLocalLine + int64(len(p.Lines)) - 1
			p.CLICommand = BuildCLICommand("samples", map[string]any{
				"path": paths[p.Part], "lines": fmt.Sprintf("%d-%d", p.FirstLocalLine, last), "file_tz": fileTZ,
			})
		}
	}
}

// chainConflict is the 409 of a chain samples request: the chain's
// current description, as GET /v1/logs/chain gives it.
func (s *Server) chainConflict(d *logchain.Description, in *logSamplesInput) *logSamplesOutput {
	resp := d.Response
	resp.CLICommand = BuildCLICommand("log_chain", map[string]any{
		"path": resp.Path, "file_tz": in.FileTZ, "fingerprint": in.Fingerprint,
	})
	s.chainIndex.fillIndexBuild(d)
	return &logSamplesOutput{Status: http.StatusConflict, Body: resp}
}

// maxChainWaits bounds how many index tasks of a chain one samples
// request waits for. A rotation during the wait leaves the chain
// pending again, with a new task, and the next description nearly
// always finds the files at rest; the bound stops the work when they
// keep moving.
const maxChainWaits = 3

// awaitChainReady waits for the index task of d, a pending chain, and
// describes the chain again, until it is not pending. It returns the
// new description, or the answer the request gives instead: 202 with the
// task when the wait passes the request's limit (Prefer: respond-async),
// 409 when the files changed under a fingerprint the client sent. It
// fails (500) when the chain's last index task failed for these files,
// which no wait mends (POST /v1/logs/index starts another), and when the
// chain is still pending after maxChainWaits tasks; and with 503 when no
// task runs for the chain and none can start, as many chain index tasks
// as the server runs at once being unfinished (chainIndexTasks.start).
//
// The task goroutine builds the parts; this request goroutine only
// waits in a select on the task's done channel, its own context and the
// limit's timer, whichever comes first, as samplesIndexBuilds.await
// does for one file.
func (s *Server) awaitChainReady(ctx context.Context, in *logSamplesInput, opts logchain.Options,
	d *logchain.Description, limit waitLimit) (*logchain.Description, *logSamplesOutput, error) {
	for wait := 0; d.Response.State == rxtypes.ChainStatePending; wait++ {
		build, refused := s.chainIndex.forDescription(d)
		if refused {
			return nil, nil, chainTasksFullError(d.Response.Path, s.chainIndex.maxUnfinished())
		}
		if build == nil || build.Status == string(tasks.StatusFailed) || wait == maxChainWaits {
			return nil, nil, ErrInternal(chainNotReadyDetail(d, build))
		}
		if done, known := s.cfg.TaskManager.Done(build.TaskID); known {
			deadline, stop := limit.timer()
			// A receive from a nil channel blocks for ever, so without
			// respond-async only the task's end or the client's leaving
			// ends the wait.
			select {
			case <-done:
			case <-ctx.Done():
				stop()
				return nil, nil, fmt.Errorf("waiting for the index task of %s: %w", d.Response.Path, ctx.Err())
			case <-deadline:
				stop()
				return nil, &logSamplesOutput{
					Status: http.StatusAccepted, PreferenceApplied: respondAsync, Body: taskResponseOfBuild(build),
				}, nil
			}
			stop()
		}
		next, changed, err := logchain.DescribeHandle(ctx, in.Path, opts)
		if err != nil {
			return nil, nil, logChainError(in.Path, err)
		}
		if logChainStatus(changed, in.Fingerprint, next.Response.Fingerprint) != http.StatusOK {
			return nil, s.chainConflict(next, in), nil
		}
		d = next
	}
	return d, nil, nil
}

// chainNotReadyDetail says why a pending chain cannot be read by global
// line or time now.
func chainNotReadyDetail(d *logchain.Description, build *rxtypes.SamplesIndexBuild) string {
	if build != nil && build.Status == string(tasks.StatusFailed) {
		return fmt.Sprintf("The chain %s is pending and cannot be read by global line or time: %s", d.Response.Path, build.Message)
	}
	return fmt.Sprintf("The chain %s is still pending after %d index tasks; its files keep changing", d.Response.Path, maxChainWaits)
}

// taskResponseOfBuild is the 202 body that names a running build.
func taskResponseOfBuild(build *rxtypes.SamplesIndexBuild) rxtypes.TaskResponse {
	return rxtypes.TaskResponse{
		TaskID: build.TaskID, Status: build.Status, Message: build.Message, Path: build.Path, StartedAt: build.StartedAt,
	}
}

// waitLimit is how long one request may wait, in all, for the
// background work it needs (the chain's index task, then a part's index
// build) before it answers 202: RX_SAMPLES_WAIT_SECONDS from the
// request's start for a client that prefers respond-async, and no limit
// for any other.
type waitLimit struct {
	async bool
	until time.Time
}

// newWaitLimit is the limit of a request with the Prefer header prefer.
func newWaitLimit(prefer string, wait time.Duration) waitLimit {
	if !prefersRespondAsync(prefer) {
		return waitLimit{}
	}
	return waitLimit{async: true, until: time.Now().Add(wait)}
}

// timer returns a channel that delivers when the limit is reached, nil
// (never) without a limit, and what releases it. Each wait takes its
// own timer for what is left of the limit, so two waits in one request
// share it.
func (w waitLimit) timer() (<-chan time.Time, func()) {
	if !w.async {
		return nil, func() {}
	}
	t := time.NewTimer(max(0, time.Until(w.until)))
	return t.C, func() { t.Stop() }
}

// chainParts is the reader of a chain samples request's parts over
// HTTP, and the background build an answer from a part's head started.
type chainParts struct {
	s       *Server
	limit   waitLimit
	noIndex bool
	// built is the first part build an answer from a part's head
	// started or joined, for the answer's index_build.
	built *rxtypes.SamplesIndexBuild
}

// chainPartReader returns the reader of a request's parts: each part
// read as GET /v1/samples reads one file, within limit.
func (s *Server) chainPartReader(limit waitLimit) *chainParts {
	return &chainParts{s: s, limit: limit, noIndex: config.GetBoolEnv("RX_NO_INDEX", false)}
}

// errPartPending is returned by chainParts.read when a part's index
// build outlasts the request's limit: the request answers 202 with the
// build's task.
type errPartPending struct {
	task rxtypes.TaskResponse
}

func (e *errPartPending) Error() string { return e.task.Message }

// read implements logchain.PartReader: req on one part, through the
// samples path of one file (readFileSamples), with a current stat of
// the part taken through its pin, which refuses a name that leads to
// another file by now.
func (c *chainParts) read(ctx context.Context, part logchain.Part, req samples.Request) (*rxtypes.SamplesResponse, error) {
	stat, err := part.File.Stat()
	if err != nil {
		return nil, err
	}
	out, err := c.s.readFileSamples(ctx, req, stat, c.noIndex, c.limit.timer)
	if err != nil {
		return nil, err
	}
	if out.pending != nil {
		return nil, &errPartPending{task: *out.pending}
	}
	if c.built == nil {
		c.built = out.indexBuild
	}
	return out.resp, nil
}

// logSamplesError is the answer to a chain samples request that failed
// while it read the parts: 202 for a part build that outlasted the
// limit, 409 with the current description for a part that changed
// after the description, 422 for an invalid chain, 400 for a request at
// fault (a part that is not a member, a time named wrongly, an answer
// over the limits, a part rx refuses to decompress), 500 otherwise.
func (s *Server) logSamplesError(ctx context.Context, in *logSamplesInput, opts logchain.Options, err error) (*logSamplesOutput, error) {
	var pending *errPartPending
	if errors.As(err, &pending) {
		return &logSamplesOutput{Status: http.StatusAccepted, PreferenceApplied: respondAsync, Body: pending.task}, nil
	}
	if errors.Is(err, paths.ErrFileChanged) || errors.Is(err, fs.ErrNotExist) {
		d, _, describeErr := logchain.DescribeHandle(ctx, in.Path, opts)
		if describeErr != nil {
			return nil, logChainError(in.Path, describeErr)
		}
		return s.chainConflict(d, in), nil
	}
	if setting := answerLimitSetting(err); setting != "" {
		return nil, ErrBadRequest(fmt.Sprintf(
			"%s; %s sets the limit: ask for fewer positions, shorter ranges or less context", err.Error(), setting))
	}
	switch {
	case errors.Is(err, logchain.ErrChainInvalid):
		return nil, &apiError{Status: http.StatusUnprocessableEntity, Detail: err.Error()}
	case errors.Is(err, logchain.ErrNotAPart), errors.Is(err, logchain.ErrSamplesRequest), samples.IsUsageError(err):
		return nil, ErrBadRequest(err.Error())
	case errors.Is(err, compression.ErrTooLargeToDecode):
		return nil, ErrBadRequest(fmt.Sprintf("%s: %s", compression.TooLargeToDecodeReason, in.Path))
	}
	return nil, ErrInternal(err.Error())
}
