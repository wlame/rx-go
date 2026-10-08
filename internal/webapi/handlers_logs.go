package webapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logChainsInput is the query string of GET /v1/logs/chains.
type logChainsInput struct {
	Path string `query:"path" required:"true" example:"/var/log" doc:"The directory whose log chains to list."`
}

// logChainsOutput wraps the answer for huma, which takes the body from
// the Body field.
type logChainsOutput struct {
	Body rxtypes.ChainsResponse
}

// registerLogsHandlers mounts the /v1/logs routes: the log chains of a
// directory, which `rx logs list` gives from a terminal, the
// description of one chain, which `rx logs show` gives, and its index
// task, which `rx logs index` does in the foreground.
//
// GET /v1/logs/chains answers from the directory's names alone, as
// GET /v1/tree lists it: one listing, the text check of the names that
// can form a chain, and a peek at the stored line index of each frozen
// part. It reads no part's text beyond that check and writes nothing.
func registerLogsHandlers(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "log_chains",
		Method:      http.MethodGet,
		Path:        "/v1/logs/chains",
		Summary:     "List the log chains of a directory",
		Description: "The log chains of one directory: the files of each rotated log (syslog, syslog.1, " +
			"syslog.2.gz, …), found from their names alone, with each chain's parts in the provisional order " +
			"(by the number or date in the names, oldest first), the numbers missing between them, their total " +
			"size, compression formats, and whether every part but the active file has a line index. " +
			"A file of one generation in several encodings is one part. Directories, hidden entries, .tmp " +
			"files and files that are not text are no parts. Errors as GET /v1/tree for the same path.",
		Tags:      []string{"Logs"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(_ context.Context, in *logChainsInput) (*logChainsOutput, error) {
		resp, err := logchain.List(in.Path)
		if err != nil {
			return nil, logChainsError(in.Path, err)
		}
		return &logChainsOutput{Body: *resp}, nil
	})
	registerLogChainHandler(s, api)
	registerLogIndexHandler(s, api)
}

// logChainsError is the answer to a directory GET /v1/logs/chains
// cannot list, with the status GET /v1/tree gives for the same path:
// 403 outside the roots or into a hidden entry, 404 for one that does
// not exist, 400 for a file, and 403 for one the process may not read.
func logChainsError(path string, err error) huma.StatusError {
	shown := path
	if abs, absErr := filepath.Abs(path); absErr == nil {
		shown = abs
	}
	var outside *paths.ErrPathOutsideRoots
	switch {
	case errors.As(err, &outside):
		return NewSandboxError(outside)
	case errors.Is(err, logchain.ErrNotADirectory):
		return ErrBadRequest(fmt.Sprintf("Path is not a directory: %s", shown))
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound(fmt.Sprintf("Path not found: %s", shown))
	}
	var hidden *paths.ErrHiddenPath
	if errors.As(err, &hidden) {
		return ErrForbidden(err.Error())
	}
	return ErrFileAccess(shown, err)
}

// logChainInput is the query string of GET /v1/logs/chain.
type logChainInput struct {
	Path        string `query:"path" required:"true" example:"/var/log/syslog" doc:"The chain's handle: its directory joined with its name, as GET /v1/logs/chains gives it in path."`
	FileTZ      string `query:"file_tz" example:"Europe/Berlin" doc:"Read every part's timestamps as the wall clock each line writes, in this zone: UTC, an IANA zone name or ±HH:MM (as RX_LOG_TZ takes it), as GET /v1/time-range reads one file. Empty or absent: each part is read as its timestamps say. Another value is refused with 400."`
	Fingerprint string `query:"fingerprint" pattern:"^[0-9a-f]{16}$" example:"3fa2c4d5e6f70812" doc:"The fingerprint of a description the client holds. When the chain's files changed since (the fingerprint differs), the answer is 409 with the current description."`
}

// logChainOutput is the answer of GET /v1/logs/chain: the description,
// with 200, or with 409 when the client's fingerprint no longer matches
// or a part changed while the request read it.
//
// huma reads the status from the Status field. Body is `any`, as in
// samplesOutput, so the document takes both statuses' schemas from the
// declared responses (logChainResponses).
type logChainOutput struct {
	Status int
	Body   any
}

// logChainResponses declares the two answers that carry a description
// beside the error answers of GET /v1/logs/chain.
func logChainResponses(api huma.API) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	description := registry.Schema(reflect.TypeOf(rxtypes.ChainResponse{}), true, "ChainResponse")
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusInternalServerError)
	responses[strconv.Itoa(http.StatusOK)] = jsonResponse("The chain's description, whatever its state: "+
		"pending, ready or invalid (with the reasons).", description)
	responses[strconv.Itoa(http.StatusConflict)] = jsonResponse("The chain's files changed: the fingerprint "+
		"the request sent differs from the current one, or a part was renamed or replaced while the request "+
		"read it. The body is the current description.", description)
	return responses
}

// registerLogChainHandler mounts GET /v1/logs/chain, the description of
// one chain, which `rx logs show` gives from a terminal.
//
// The request reads each frozen part's stored line index and the head
// and the tail of the active file (samples.TimeRange): never a whole
// part. A frozen part without an index is opened, to learn that it can
// be read, and left unread; the chain is then pending, and the request
// starts the chain's index task in the background (or joins it), which
// index_build names (chainIndexTasks.forDescription). The request does
// not wait for it.
func registerLogChainHandler(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "log_chain",
		Method:      http.MethodGet,
		Path:        "/v1/logs/chain",
		Summary:     "Describe one log chain",
		Description: "The description of one log chain: its parts in time order with their line counts, " +
			"first and highest timestamps and global starts, the checks that make it valid (every part with lines " +
			"has timestamps, neighboring parts overlap by at most RX_CHAIN_OVERLAP_SECONDS, the active file comes " +
			"last, every part can be read, at most 10,000 parts), its state (pending until every frozen part has a " +
			"line index, ready, or invalid with the reasons), the time gaps and missing parts, and a fingerprint of " +
			"its files. From the parts' line indexes and the head and tail of the active file only. A pending chain " +
			"starts its index task in the background (or joins the running one), named in index_build. 409 with the " +
			"current description when fingerprint differs or a part changed while it was read; 404 when the handle " +
			"names fewer than two parts.",
		Tags:      []string{"Logs"},
		Responses: logChainResponses(api),
	}, func(ctx context.Context, in *logChainInput) (*logChainOutput, error) {
		fileZone, err := fileZoneOf(in.FileTZ)
		if err != nil {
			return nil, err
		}
		// ctx ends when the client disconnects, which stops the reads.
		d, changed, err := logchain.DescribeHandle(ctx, in.Path, logchain.Options{FileZone: fileZone})
		if err != nil {
			return nil, logChainError(in.Path, err)
		}
		resp := d.Response
		resp.CLICommand = BuildCLICommand("log_chain", map[string]any{
			"path": resp.Path, "file_tz": in.FileTZ, "fingerprint": in.Fingerprint,
		})
		resp.IndexBuild = s.chainIndex.forDescription(d)
		return &logChainOutput{Status: logChainStatus(changed, in.Fingerprint, resp.Fingerprint), Body: resp}, nil
	})
}

// partCount words a number of parts: "1 part", "3 parts".
func partCount(n int) string {
	if n == 1 {
		return "1 part"
	}
	return strconv.Itoa(n) + " parts"
}

// logIndexInput is the query string of POST /v1/logs/index.
type logIndexInput struct {
	Path        string `query:"path" required:"true" example:"/var/log/syslog" doc:"The chain's handle: its directory joined with its name, as GET /v1/logs/chains gives it in path."`
	Force       bool   `query:"force" doc:"Build the index of every part again, current ones too. A request that joins the chain's running task does not change what that task builds."`
	Fingerprint string `query:"fingerprint" pattern:"^[0-9a-f]{16}$" example:"3fa2c4d5e6f70812" doc:"The fingerprint of a description the client holds. When the chain's files changed since (the fingerprint differs), the answer is 409 with the current description, and no task starts."`
}

// logIndexResponses declares the answers of POST /v1/logs/index: the
// task, or the current description with 409, beside the error answers.
func logIndexResponses(api huma.API) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	description := registry.Schema(reflect.TypeOf(rxtypes.ChainResponse{}), true, "ChainResponse")
	task := registry.Schema(reflect.TypeOf(rxtypes.TaskResponse{}), true, "TaskResponse")
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusInternalServerError)
	responses[strconv.Itoa(http.StatusOK)] = jsonResponse("The chain's index task: started, or the one already "+
		"running for the chain, joined. Follow it at GET /v1/tasks/{task_id}.", task)
	responses[strconv.Itoa(http.StatusConflict)] = jsonResponse("The chain's files changed: the fingerprint the "+
		"request sent differs from the current one, or a part was renamed or replaced while the request read it. "+
		"The body is the current description; no task started.", description)
	return responses
}

// registerLogIndexHandler mounts POST /v1/logs/index, which starts the
// index task of one chain, or joins the one running for it: the
// background form of `rx logs index`.
//
// The request describes the chain as GET /v1/logs/chain does (no part
// read in full) to learn its parts and which have a current index, and
// returns at once; the task builds the indexes. Without force it builds
// every part without a current index, the active file too, whose index
// speeds a jump inside it; with force, every part.
func registerLogIndexHandler(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "log_index",
		Method:      http.MethodPost,
		Path:        "/v1/logs/index",
		Summary:     "Index every part of one log chain (background task)",
		Description: "Starts the index task of one log chain (operation chain_index), or joins the one running for " +
			"it: it builds and stores the line index of every part without a current one, the active file " +
			"too (with force=true, of every part), whatever a part's size, at most RX_MAX_INDEX_BUILDS at a " +
			"time. Poll GET /v1/tasks/{task_id}: its progress is the share of parts done. 409 with the current " +
			"description, and no task, when fingerprint differs or a part changed while it was read; 404 when " +
			"the handle names fewer than two parts.",
		Tags:      []string{"Logs"},
		Responses: logIndexResponses(api),
	}, func(ctx context.Context, in *logIndexInput) (*logChainOutput, error) {
		d, changed, err := logchain.DescribeHandle(ctx, in.Path, logchain.Options{})
		if err != nil {
			return nil, logChainError(in.Path, err)
		}
		if status := logChainStatus(changed, in.Fingerprint, d.Response.Fingerprint); status != http.StatusOK {
			resp := d.Response
			resp.CLICommand = BuildCLICommand("log_chain", map[string]any{"path": resp.Path, "fingerprint": in.Fingerprint})
			resp.IndexBuild = s.chainIndex.last(d)
			return &logChainOutput{Status: status, Body: resp}, nil
		}
		parts := d.UnindexedParts()
		if in.Force {
			parts = d.Parts()
		}
		task, isNew := s.chainIndex.start(chainTaskStart{
			handle: d.Response.Path, key: chainKeyOf(d), fingerprint: d.Response.Fingerprint, parts: parts, force: in.Force,
		})
		message := fmt.Sprintf("Indexing %s of the log chain %s; follow GET /v1/tasks/%s",
			partCount(len(parts)), d.Response.Path, task.TaskID)
		if !isNew {
			message = fmt.Sprintf("Joined the index task already running for the log chain %s, which builds "+
				"what it was started for; follow GET /v1/tasks/%s", d.Response.Path, task.TaskID)
		}
		started := formatTaskTime(task.StartedAt)
		return &logChainOutput{Status: http.StatusOK, Body: rxtypes.TaskResponse{
			TaskID: task.TaskID, Status: string(task.Status), Path: task.Path, StartedAt: &started, Message: message,
		}}, nil
	})
}

// logChainStatus is the status of a description: 409 when a part
// changed while the request read it (changed), or when the client sent
// a fingerprint (sent) that is not the chain's now (current); 200
// otherwise.
func logChainStatus(changed bool, sent, current string) int {
	if changed || (sent != "" && sent != current) {
		return http.StatusConflict
	}
	return http.StatusOK
}

// logChainError is the answer to a handle GET /v1/logs/chain cannot
// describe: 400 for a handle that ends in no name, 403 outside the
// roots or into a hidden entry (as GET /v1/tree), 404 when it names no
// chain or its directory does not exist, and 500 when the chain's files
// kept changing for every attempt to read them.
func logChainError(path string, err error) huma.StatusError {
	switch {
	case errors.Is(err, logchain.ErrInvalidHandle):
		return ErrBadRequest(fmt.Sprintf("%s: %s", err.Error(), path))
	case errors.Is(err, logchain.ErrNotAChain):
		return ErrNotFound(fmt.Sprintf("Not a log chain (fewer than two parts): %s", path))
	case errors.Is(err, logchain.ErrPartChanged):
		return ErrInternal(fmt.Sprintf("The files of the chain kept changing while they were read: %s", path))
	}
	return logChainsError(path, err)
}
