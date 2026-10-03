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
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/prometheus"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/tasks"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// findingsOperation is the operation name of a findings task.
const findingsOperation = "findings"

// maxSelectionBytes bounds a selection a request sends (signatures=):
// room for a few hundred ids, far more than any selection a person
// writes. It keeps the work of resolving one, and the message that
// quotes it back, small.
const maxSelectionBytes = 4096

// defaultFindingsLimit is the page size of a findings answer when the
// request names none, and the size of the page a findings task's
// result holds.
const defaultFindingsLimit = 1000

// maxUnfinishedScans is how many findings tasks may be queued or
// running at once, the size of the line-index build queue. Each holds a
// goroutine that waits for the scan queue (RX_MAX_SIGNATURE_SCANS), so
// without the bound a client that names every file of a search root
// would hold a goroutine per file.
const maxUnfinishedScans = maxQueuedIndexBuilds

// scansFullRetryAfterSeconds is the Retry-After of the 503 a scan past
// maxUnfinishedScans gets.
const scansFullRetryAfterSeconds = 5

// findingsInput is the query string of GET /v1/findings. Every filter
// applies before the page is cut, so offset and limit page through the
// filtered list.
type findingsInput struct {
	Path       string   `query:"path" required:"true" example:"/var/log/app.log" doc:"The file whose findings to give. Must be inside a configured --search-root."`
	Signatures string   `query:"signatures" maxLength:"4096" example:"traceback,sql" doc:"The selection the summary describes (state, checked, missing), as rx trace --signatures= takes it: comma-separated ids, groups and tags, all, default, -word to remove. Empty or absent: the default set. A selection that names an unknown or refused signature is refused with 400 and the load error."`
	Signature  []string `query:"signature,explode" maxItems:"256" doc:"Only the findings of these signatures (repeatable, at most 256: each costs a comparison per finding)."`
	Group      []string `query:"group,explode" maxItems:"256" doc:"Only the findings of signatures of these groups (repeatable, at most 256)."`
	Q          string   `query:"q" maxLength:"1024" doc:"Only the findings whose excerpt or signature name holds this text, compared without case. On a server that masks secrets the excerpt is masked already, so a masked value is never matched."`
	Lines      string   `query:"lines" example:"1000-1200" doc:"Only the findings that overlap lines A-B (1-based, inclusive); one number N means N-N."`
	AfterLine  int64    `query:"after_line" minimum:"0" doc:"Only the findings whose first line is after line N (the next finding). 0 or absent: no bound."`
	BeforeLine int64    `query:"before_line" minimum:"0" doc:"Only the findings whose first line is before line N, listed from the nearest down, in the reverse order (the previous finding). 0 or absent: no bound."`
	Offset     int      `query:"offset" minimum:"0" doc:"How many filtered findings to pass over before the page."`
	Limit      int      `query:"limit" minimum:"1" maximum:"10000" default:"1000" doc:"The most findings in the page."`
	Text       bool     `query:"text" doc:"Fill lines (and before) of the page's findings, read from the file and, on a server that masks secrets, masked as every output of findings is: a value a redacting signature matches is masked and a body line of a redacting block or run shows as [masked, N bytes]. No line past the text the scan read is shown."`
	Before     int      `query:"before" minimum:"0" maximum:"5" doc:"With text: how many lines before each finding to give in before."`
	MaxBytes   int64    `query:"max_bytes" minimum:"0" doc:"With text: the most bytes of text the page holds, taken in line order; the finding at which it runs out and every later one lose their lines and are marked lines_truncated. 0 or absent: RX_FINDINGS_TEXT_MAX_BYTES, which also bounds a larger value."`
	Ticks      int      `query:"ticks" minimum:"0" maximum:"4000" doc:"Place the filtered findings on a timeline of this many columns over the file's time range (ticks). 0 or absent: no ticks."`
	FileTZ     string   `query:"file_tz" example:"Europe/Berlin" doc:"Read the file's timestamps as the wall clock each line writes, in this zone: UTC, an IANA zone name or ±HH:MM, as GET /v1/samples and GET /v1/time-range take it. It moves time_ms, series and ticks as it moves their times; a file whose timestamps carry zones needs its line index to record where their offset changes, and its findings have no time without it. Another value is refused with 400."`
}

// findingsOutput wraps a findings answer for huma.
type findingsOutput struct {
	Body rxtypes.FindingsResponse
}

// findingsScanInput is the body of POST /v1/findings.
type findingsScanInput struct {
	Body rxtypes.FindingsScanRequest
}

// findingsScanOutput is the 202 answer of POST /v1/findings.
type findingsScanOutput struct {
	Body rxtypes.TaskResponse
}

// registerFindingsHandlers mounts GET /v1/findings, which answers from
// the findings cache alone, and POST /v1/findings, which scans a file
// in a background task.
func registerFindingsHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "findings",
		Method:      http.MethodGet,
		Path:        "/v1/findings",
		Summary:     "Get the cached findings of one file",
		Description: "The findings the findings cache holds for one file; it never scans. Every answer has the " +
			"summary of the requested selection (state, checked, missing, counts, series), total (the findings " +
			"that pass the filters) and cli_command; findings is one page of the filtered list, and ticks, with " +
			"ticks=W, places the filtered findings on a timeline. Filters: signature, group, q, lines, after_line, " +
			"before_line (descending). A missing signature is scanned by POST /v1/findings.",
		Tags: []string{"Signatures"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusInternalServerError),
	}, func(ctx context.Context, in *findingsInput) (*findingsOutput, error) {
		return s.getFindings(ctx, in)
	})

	huma.Register(api, huma.Operation{
		OperationID:   "findings-scan",
		Method:        http.MethodPost,
		Path:          "/v1/findings",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Scan a file, or every file of a directory, for signatures (background task)",
		Description: "Starts a task (operation findings) that scans the file for the selected signatures the " +
			"findings cache does not hold (with force, for all of them) and caches what it finds; a scan of the " +
			"same file already running is returned instead, and it scans what it was started for. Poll " +
			"GET /v1/tasks/{task_id}: bytes_read and bytes_total say how far it read; its result is the answer " +
			"GET /v1/findings gives without filters, with the first page. DELETE /v1/tasks/{task_id} cancels it. " +
			"At most RX_MAX_SIGNATURE_SCANS scans read files at once; later ones wait, queued. For a directory, " +
			"one task scans every file the file tree lists under it, recursively, one at a time, as " +
			"rx scan DIR --recursive does: a file that is not text, and a file whose findings are current for " +
			"every selected signature (unless force), is skipped, a file whose scan fails is recorded and the " +
			"task goes on. Its status has files_done and files_total (bytes_read and bytes_total follow the " +
			"file it reads), its result is a FolderTaskResult, and DELETE cancels it between two files or " +
			"inside one; the files scanned before the cancel keep their findings.",
		Tags:      []string{"Signatures"},
		Responses: findingsScanResponses(api),
	}, func(_ context.Context, in *findingsScanInput) (*findingsScanOutput, error) {
		return s.startFindingsScan(in.Body)
	})
}

// getFindings answers GET /v1/findings: the file checked and classified
// as a search classifies it, the selection resolved, the cache read
// (findings.Read), and the answer cut by the request's filters, page,
// text and ticks (findingsAnswer).
func (s *Server) getFindings(ctx context.Context, in *findingsInput) (*findingsOutput, error) {
	validated, err := findingsPath(in.Path)
	if err != nil {
		return nil, err
	}
	fileZone, err := fileZoneOf(in.FileTZ)
	if err != nil {
		return nil, err
	}
	lines, err := parseLineRange(in.Lines)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	sel, err := s.selectionOf(in.Signatures)
	if err != nil {
		return nil, err
	}
	file, err := findingsFile(validated)
	if err != nil {
		return nil, err
	}
	answer, err := findings.Read(file, s.cfg.Signatures, sel, s.cfg.Findings.Scan)
	if err != nil {
		return nil, findingsFailure(validated, err)
	}
	prometheus.AddFindingsCacheHits(answer.Cached)

	view := findingsView{
		filter: findingsFilter{
			signatures: in.Signature, groups: in.Group, query: strings.ToLower(in.Q), lines: lines,
			afterLine: in.AfterLine, beforeLine: in.BeforeLine,
		},
		offset: in.Offset, limit: in.Limit, text: in.Text, before: in.Before, maxBytes: in.MaxBytes,
		ticks: in.Ticks, fileZone: fileZone,
	}
	resp, err := s.findingsAnswer(ctx, validated, file, answer, sel, view)
	if err != nil {
		return nil, findingsFailure(validated, err)
	}
	params := s.findingsCLIParams(validated, in.Signatures)
	if in.Text && in.Before > 0 {
		before := in.Before
		params["before_context"] = &before
	}
	resp.CLICommand = BuildCLICommand("trace", params)
	return &findingsOutput{Body: *resp}, nil
}

// startFindingsScan answers POST /v1/findings: it checks the path and
// the selection as GET /v1/findings does, then starts the scan task of
// the file, or of every file under a directory (runFindingsFolder), or
// joins the one running for the same path. The task runs detached from
// the request (runDetached), under the context its task keeps, so only
// DELETE /v1/tasks/{task_id} stops it.
func (s *Server) startFindingsScan(req rxtypes.FindingsScanRequest) (*findingsScanOutput, error) {
	validated, err := findingsPath(req.Path)
	if err != nil {
		return nil, err
	}
	sel, err := s.selectionOf(req.Signatures)
	if err != nil {
		return nil, err
	}
	src, err := paths.Pin(validated)
	if err != nil {
		return nil, ErrFileAccess(validated, err)
	}
	cliParams := map[string]any{
		"path": validated, "signatures": selectionText(req.Signatures), "force": req.Force,
		"mask_secrets": s.cfg.Findings.Scan.MaskSecrets,
	}
	if src.Info().IsDir() {
		cliParams["recursive"] = true
		cli := BuildCLICommand("scan", cliParams)
		return s.startScanTask(validated, cli, func(taskID string) string {
			return fmt.Sprintf("Scanning every file under %s for %d signatures; follow GET /v1/tasks/%s", validated, len(sel), taskID)
		}, func(ctx context.Context, taskID string) {
			s.runFindingsFolder(ctx, folderScan{taskID: taskID, dir: src, sel: sel, force: req.Force, cli: cli})
		})
	}
	file, err := findingsFileOf(validated, src)
	if err != nil {
		return nil, err
	}
	scan := findingsScan{
		path: validated, file: file, sel: sel, force: req.Force,
		cli: BuildCLICommand("trace", s.findingsCLIParams(validated, req.Signatures)),
	}
	return s.startScanTask(validated, BuildCLICommand("scan", cliParams), func(taskID string) string {
		return fmt.Sprintf("Scanning %s for %d signatures; follow GET /v1/tasks/%s", validated, len(sel), taskID)
	}, func(ctx context.Context, taskID string) {
		scan.taskID = taskID
		s.runFindingsScan(ctx, scan)
	})
}

// startScanTask starts a findings task for validated (a file or a
// directory) whose command is cli, or joins the one already running
// for the path. message words the answer of a new task; work is its
// worker, run detached under the task's context.
func (s *Server) startScanTask(validated, cli string, message func(taskID string) string,
	work func(ctx context.Context, taskID string)) (*findingsScanOutput, error) {
	mgr := s.cfg.TaskManager
	// A place among the unfinished scans is taken before the task is
	// created, and given back when the task joins one already there or
	// when its worker returns.
	//
	// Go note: the channel is a semaphore. Its buffer holds one value
	// per place; a send takes a place, and the select's default case
	// runs instead of blocking when every place is taken.
	select {
	case s.scanPlaces <- struct{}{}:
	default:
		// No place for a new task; a scan of the path already there is
		// still joined.
		if running, ok := mgr.CancellableHolder(validated, findingsOperation); ok {
			return joinedScan(validated, running), nil
		}
		return nil, huma.ErrorWithHeaders(ErrServiceUnavailable(fmt.Sprintf(
			"%d signature scans are queued or running, as many as the server holds; ask again once one has ended",
			cap(s.scanPlaces))), http.Header{"Retry-After": {strconv.Itoa(scansFullRetryAfterSeconds)}})
	}
	task, taskCtx, isNew := mgr.CreateCancellable(validated, findingsOperation)
	if !isNew {
		<-s.scanPlaces
		// The running task is the manager's own: read it through Get,
		// a copy taken under the manager's lock, since its worker
		// updates it meanwhile.
		running, ok := mgr.Get(task.TaskID)
		if !ok {
			return nil, ErrNotFound(fmt.Sprintf("Task not found: %s", task.TaskID))
		}
		return joinedScan(validated, running), nil
	}
	mgr.SetCLICommand(task.TaskID, cli)

	// Snapshot the new task before its worker starts: from then on the
	// worker updates it, and reading it here would be a data race.
	taskID, status, started := task.TaskID, string(task.Status), formatTaskTime(task.StartedAt)
	// Go note: the goroutine outlives this request. runDetached turns a
	// panic in it into a failed task instead of a crashed server.
	go runDetached(mgr, taskID, findingsOperation, s.cfg.Logger, func() {
		// Go note: a deferred call runs when the function returns, and
		// also while a panic unwinds it, before runDetached recovers, so
		// the place is given back on every path.
		defer func() { <-s.scanPlaces }()
		// A panic in the scan fails the task in runDetached, which
		// recovers it; it is counted here first, then passed on.
		//
		// Go note: recover stops a panic only inside a deferred call;
		// panic(r) starts it again, so runDetached's own deferred
		// recover still sees it and fails the task.
		defer func() {
			if r := recover(); r != nil {
				prometheus.RecordSignatureScan("failed", false, 0)
				panic(r)
			}
		}()
		work(taskCtx, taskID)
	})
	return &findingsScanOutput{Body: rxtypes.TaskResponse{
		TaskID: taskID, Status: status, Path: validated, StartedAt: &started, CLICommand: cli,
		Message: message(taskID),
	}}, nil
}

// joinedScan is the answer of a POST /v1/findings that joins running,
// the scan of the file or directory already queued or running: a copy
// of the task, which the caller took from the manager, with its
// command.
func joinedScan(validated string, running *tasks.Task) *findingsScanOutput {
	started := formatTaskTime(running.StartedAt)
	return &findingsScanOutput{Body: rxtypes.TaskResponse{
		TaskID: running.TaskID, Status: string(running.Status), Path: running.Path, StartedAt: &started,
		CLICommand: running.CLICommand,
		Message: fmt.Sprintf("Joined the signature scan already running for %s, which scans what it was "+
			"started for; follow GET /v1/tasks/%s", validated, running.TaskID),
	}}
}

// findingsScanResponses declares the answers of POST /v1/findings: the
// error answers, and the 503 of a scan past maxUnfinishedScans with its
// Retry-After header. The shared 503 description is about ripgrep,
// which this route does not run.
func findingsScanResponses(api huma.API) map[string]*huma.Response {
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity)
	envelope := api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(apiError{}), true, "ApiError")
	full := jsonResponse(fmt.Sprintf("No task started: %d signature scans are queued or running, as many as the "+
		"server holds. Ask again once one has ended.", maxUnfinishedScans), envelope)
	full.Headers = map[string]*huma.Param{
		"Retry-After": {
			Description: fmt.Sprintf("%d: the seconds to wait before asking again (RFC 9110).", scansFullRetryAfterSeconds),
			Schema:      &huma.Schema{Type: huma.TypeInteger},
		},
	}
	responses[strconv.Itoa(http.StatusServiceUnavailable)] = full
	return responses
}

// findingsScan is one findings task's work: the file, the selection,
// force, and the cli_command of its result.
type findingsScan struct {
	taskID, path string
	file         trace.SearchFile
	sel          []*signatures.Signature
	force        bool
	cli          string
}

// scanProgress is how far a findings task has read, written by the
// scan's goroutines and read by the goroutines that serve status
// requests, so every member is atomic.
type scanProgress struct {
	read, total atomic.Int64
	started     atomic.Bool
}

// bytes is the task's ByteProgressFunc.
func (p *scanProgress) bytes() (int64, int64) { return p.read.Load(), p.total.Load() }

// fraction is the task's ProgressFunc: the share of the file read.
func (p *scanProgress) fraction() (float64, bool) {
	total := p.total.Load()
	if total <= 0 {
		return 0, false
	}
	return min(float64(p.read.Load())/float64(total), 1), true
}

// runFindingsScan is the worker of a findings task. The task stays
// queued while it waits for its file's scan or a place in the scan
// queue, and is running from the first bytes it reads, which also
// start its progress; a scan the cache answers whole goes from queued
// to completed. The task completes with the answer GET /v1/findings
// gives without filters (the first page), or fails with the scan's
// error. A cancel (DELETE /v1/tasks/{task_id}) ends the task at once
// and cancels ctx: the scan stops and writes nothing, and this worker
// leaves the task as Cancel left it. Once the scan has its answer the
// task settles (tasks.Manager.Settle, through ScanOptions.MayWrite),
// and a cancel is refused from then on.
func (s *Server) runFindingsScan(ctx context.Context, scan findingsScan) {
	mgr := s.cfg.TaskManager
	start := time.Now()
	progress := &scanProgress{}
	opts := s.cfg.Findings.Scan
	opts.Force = scan.force
	// The scan calls this from its own goroutines as it reads.
	opts.Progress = func(read, total int64) {
		progress.read.Store(read)
		progress.total.Store(total)
		if progress.started.CompareAndSwap(false, true) {
			mgr.MarkRunning(scan.taskID)
			mgr.ReportBytes(scan.taskID, progress.bytes)
			mgr.ReportProgress(scan.taskID, progress.fraction)
		}
	}
	// The scan's point of no return: the task settles there, so a cancel
	// either comes first (the scan then writes nothing) or is refused
	// (the task completes with what the scan wrote). A task never ends
	// cancelled with entries on disk.
	opts.MayWrite = func() bool { return mgr.Settle(scan.taskID) }
	answer, err := s.cfg.scanFindings(ctx, scan.file, s.cfg.Signatures, scan.sel, opts)
	// A scan the cache answered whole was never asked; it settles here.
	if err == nil && !mgr.Settle(scan.taskID) {
		err = context.Canceled
	}
	var resp *rxtypes.FindingsResponse
	if err == nil {
		resp, err = s.findingsAnswer(ctx, scan.path, scan.file, answer, scan.sel, firstPageView())
	}
	if err == nil {
		resp.CLICommand = scan.cli
		prometheus.AddFindingsCacheHits(answer.Cached)
		mgr.Complete(scan.taskID, *resp)
		prometheus.RecordSignatureScan("completed", answer.Cached < len(scan.sel), time.Since(start))
		return
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		// Cancel ended the task already; the scan's error is the
		// cancel's.
		prometheus.RecordSignatureScan("cancelled", false, 0)
		return
	}
	mgr.Fail(scan.taskID, err.Error())
	prometheus.RecordSignatureScan("failed", false, 0)
}

// findingsView is how a findings answer is cut: its filters, its page,
// its text and its ticks, and the zone its times are read in.
type findingsView struct {
	filter        findingsFilter
	offset, limit int
	text          bool
	before        int
	maxBytes      int64
	ticks         int
	fileZone      config.Zone
}

// findingsCLIParams are the values the equivalent command of a findings
// request is written from: the path, the selection spelled out, and the
// server's masking mode, so the command gives the same findings.
func (s *Server) findingsCLIParams(path, selection string) map[string]any {
	return map[string]any{
		"path":         []string{path},
		"signatures":   selectionText(selection),
		"mask_secrets": s.cfg.Findings.Scan.MaskSecrets,
	}
}

// firstPageView is the view of a findings task's result: no filter, the
// first page of the default size, no text and no ticks.
func firstPageView() findingsView {
	return findingsView{limit: defaultFindingsLimit}
}

// findingsAnswer builds a findings answer from answer, the findings of
// sel in file (path as validated), cut by view: every stored finding of
// the checked signatures as the wire carries them, their times read in
// the view's zone (findings.WireFindings), the filters, then the page,
// whose text is read and masked as every carrier of finding text does
// (findings.FillText), and the ticks over the file's time range
// (fileTimeRange). cli_command is left to the caller.
func (s *Server) findingsAnswer(ctx context.Context, path string, file trace.SearchFile, answer *findings.Answer,
	sel []*signatures.Signature, view findingsView) (*rxtypes.FindingsResponse, error) {
	opts := *s.cfg.Findings
	opts.FileZone = view.fileZone.Location
	opts.Before, opts.After = view.before, 0
	if view.maxBytes > 0 {
		opts.TextBudget = min(opts.TextBudget, view.maxBytes)
	}
	all, series, err := findings.WireFindings(answer, file, sel, "", opts)
	if err != nil {
		return nil, err
	}
	resp := &rxtypes.FindingsResponse{
		State: string(answer.State), Checked: answer.Checked, Missing: answer.Missing,
		Counts: make(map[string]rxtypes.FindingCount, len(answer.Checked)), Series: series,
	}
	for _, id := range answer.Checked {
		res := answer.BySignature[id]
		resp.Counts[id] = rxtypes.FindingCount{Count: res.Count, Stored: len(res.Findings)}
	}
	filtered := view.filter.apply(all, s.cfg.Signatures)
	resp.Total = len(filtered)
	if view.ticks > 0 {
		first, last, err := s.fileTimeRange(ctx, path, file, view.fileZone)
		if err != nil {
			return nil, err
		}
		resp.Ticks = ticksOf(filtered, view.ticks, first, last)
	}
	resp.Findings = pageOf(filtered, view.offset, view.limit)
	if view.text {
		if err := findings.FillText(ctx, file, resp.Findings, answer, s.cfg.Signatures, opts); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// fileTimeRange is the file's first and last timestamp as
// GET /v1/time-range gives them for the same file zone (samples.TimeRange,
// the one function behind that route): the span the timeline is drawn
// over. Both are nil when it gives none, and for a file too large to
// decode for the read back, which that route refuses.
func (s *Server) fileTimeRange(ctx context.Context, path string, file trace.SearchFile, fileZone config.Zone) (*int64, *int64, error) {
	kind := file.Kind()
	resp, err := samples.TimeRange(ctx, samples.Request{
		Path: path, Source: file.Pinned(), Kind: &kind, IndexLoader: s.cfg.Findings.IndexLoader, FileZone: fileZone,
	})
	if errors.Is(err, compression.ErrTooLargeToDecode) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return resp.FirstMs, resp.LastMs, nil
}

// findingsPath checks the path of a findings request against the search
// roots: 403 outside them or into a hidden entry. Without roots (a
// server a test builds) the path is taken as it is.
func findingsPath(path string) (string, error) {
	validated, err := paths.ValidatePathWithinRoots(path)
	if err == nil {
		return validated, nil
	}
	var outside *paths.ErrPathOutsideRoots
	if errors.As(err, &outside) {
		return "", NewSandboxError(outside)
	}
	if errors.Is(err, paths.ErrNoSearchRootsConfigured) {
		return path, nil
	}
	return "", ErrForbidden(err.Error())
}

// findingsFile pins the validated path and classifies the file as a
// search does (trace.ClassifyForSearch), through the pin: 404 for a
// file that does not exist, 403 for one that cannot be read, 400 for a
// directory and for a file whose text is not text.
func findingsFile(validated string) (trace.SearchFile, error) {
	src, err := paths.Pin(validated)
	if err != nil {
		return trace.SearchFile{}, ErrFileAccess(validated, err)
	}
	return findingsFileOf(validated, src)
}

// findingsFileOf classifies the file src, which the caller pinned from
// validated, as findingsFile does.
func findingsFileOf(validated string, src paths.Pinned) (trace.SearchFile, error) {
	if src.Info().IsDir() {
		return trace.SearchFile{}, ErrBadRequest(fmt.Sprintf("Path is a directory, not a file: %s", validated))
	}
	file, err := trace.ClassifyForSearch(src)
	if err != nil {
		return trace.SearchFile{}, ErrFileAccess(validated, err)
	}
	if !file.Kind().IsText() {
		return trace.SearchFile{}, ErrBadRequest(fmt.Sprintf("%s: %s", file.Kind().NotText, validated))
	}
	return file, nil
}

// selectionOf resolves a selection a request sent against the server's
// catalogue; an empty one is the default set. A selection that names an
// unknown, refused or disabled signature, or nothing, is a 400 that
// repeats the selection's message (the close names, the load errors).
// Every route declares the parameter at most maxSelectionBytes long, so
// huma refuses a longer one first; the check here keeps the bound for
// any other caller.
func (s *Server) selectionOf(expr string) ([]*signatures.Signature, error) {
	if len(expr) > maxSelectionBytes {
		return nil, ErrBadRequest(fmt.Sprintf("signatures: at most %d bytes", maxSelectionBytes))
	}
	sel, err := s.cfg.Signatures.Resolve(selectionText(expr))
	if err != nil {
		return nil, ErrBadRequest(fmt.Sprintf("signatures=%s: %s", quotedPrefix(expr), err.Error()))
	}
	return sel, nil
}

// selectionText is the selection a request sent, "default" when it
// sent none, as rx trace --signatures= spells it.
func selectionText(expr string) string {
	if strings.TrimSpace(expr) == "" {
		return "default"
	}
	return expr
}

// findingsFailure is the answer to a findings request whose cache read,
// scan or text read failed: the file's own status when it is gone or
// cannot be read (ErrFileAccess), the request's cancel as it is, and
// 500 otherwise.
func findingsFailure(path string, err error) error {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission):
		return ErrFileAccess(path, err)
	}
	return ErrInternal("Internal error: " + err.Error())
}
