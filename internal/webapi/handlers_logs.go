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
// directory, which `rx logs list` gives from a terminal, and the
// description of one chain, which `rx logs show` gives.
//
// GET /v1/logs/chains answers from the directory's names alone, as
// GET /v1/tree lists it: one listing, the text check of the names that
// can form a chain, and a peek at the stored line index of each frozen
// part. It reads no part's text beyond that check and writes nothing.
func registerLogsHandlers(_ *Server, api huma.API) {
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
	registerLogChainHandler(api)
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
// be read, and left unread; the chain is then pending.
func registerLogChainHandler(api huma.API) {
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
			"its files. From the parts' line indexes and the head and tail of the active file only. 409 with the " +
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
		return &logChainOutput{Status: logChainStatus(changed, in.Fingerprint, resp.Fingerprint), Body: resp}, nil
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
