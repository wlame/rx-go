package webapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/logchain"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// logTraceOutput wraps the answer of GET /v1/logs/trace for huma, which
// takes the body from the Body field.
type logTraceOutput struct {
	Body rxtypes.ChainTraceResponse
}

// logTraceResponses declares the error answers of GET /v1/logs/trace:
// those of GET /v1/trace, and 409 with the error envelope for a chain
// whose files changed while the request described it.
func logTraceResponses(api huma.API) map[string]*huma.Response {
	responses := errorResponses(api, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusInternalServerError, http.StatusServiceUnavailable)
	envelope := api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(apiError{}), true, "ApiError")
	responses[strconv.Itoa(http.StatusConflict)] = jsonResponse("A part of a log chain was renamed or replaced "+
		"between the listing that found it and the read that described it (a rotation ran meanwhile); "+
		"send the request again.", envelope)
	return responses
}

// registerLogTraceHandler mounts GET /v1/logs/trace, the search of log
// chains, directories and files that `rx logs trace` gives from a
// terminal.
//
// It takes the parameters of GET /v1/trace and checks them the same way
// (prepareTrace). Each path is then a directory, whose files are
// grouped into chains, a chain's handle, or a file (logchain.Search):
// the parts of each chain are searched in the chain's order by the
// trace engine, and each match of a part is placed in its chain. Each
// chain is described from its parts' line indexes, as GET /v1/logs/chain
// describes it, but no index build starts: a chain whose parts are not
// all indexed is pending, and its matches have no chain line.
func registerLogTraceHandler(s *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "log_trace",
		Method:      http.MethodGet,
		Path:        "/v1/logs/trace",
		Summary:     "Search log chains, directories and files for regex patterns",
		Description: "GET /v1/trace for rotated logs: path repeats, and each is a directory (its files are " +
			"grouped into log chains), a chain's handle, or a file (a part's own path is a file). The parts " +
			"of each chain are searched in the chain's order (by time when it is ready, by name before), so " +
			"the file ids, the order of the matches and the cut to max_results follow it. The answer is the " +
			"trace answer plus chains, the chains found by id (c1, c2, …), and each match of a part gives its " +
			"chain and chain_line, its global line in the chain (-1 before the chain is ready, in an invalid " +
			"chain, and where the trace has no line number). Files keep their real paths and their own line " +
			"numbers; context never crosses a part's edge. Another encoding of a part is skipped " +
			"(duplicate_part), as is a part that cannot be read. No index build starts. 409 when a part of a " +
			"chain was renamed or replaced while the request described it.",
		Tags:      []string{"Logs"},
		Responses: logTraceResponses(api),
	}, func(ctx context.Context, in *traceInput) (*logTraceOutput, error) {
		prepared, err := prepareTrace(s, in)
		if err != nil {
			return nil, err
		}
		res, err := logchain.Search(ctx, s.cfg.Engine, logchain.SearchRequest{
			Paths: prepared.paths, Patterns: in.Regexp, Options: prepared.options,
		})
		if err != nil {
			return nil, logTraceError(err)
		}
		if prepared.hooks != nil {
			prepared.hooks.OnComplete(res.Trace)
		}
		answer := res.Answer
		cli := BuildCLICommand("logs_trace", prepared.cliParams(in))
		answer.CLICommand = &cli
		return &logTraceOutput{Body: *answer}, nil
	})
}

// logTraceError is the answer to a chain search that failed: 400 for a
// pattern rg cannot compile, 409 for a chain whose part changed while
// it was described, the status of a path that cannot be searched
// (logTracePathError), and 500 otherwise.
func logTraceError(err error) huma.StatusError {
	var pathErr *logchain.SearchPathError
	switch {
	case errors.Is(err, trace.ErrInvalidPattern):
		return ErrInvalidRegex(err.Error())
	case errors.Is(err, logchain.ErrPartChanged):
		return ErrConflict("A part of a log chain was renamed or replaced while the request read it; send it again")
	case errors.As(err, &pathErr):
		return logTracePathError(pathErr)
	}
	return ErrInternal(fmt.Sprintf("Internal error: %s", err.Error()))
}

// logTracePathError is the status of a path a chain search cannot
// search, as GET /v1/trace gives it: 403 outside the roots or into a
// hidden entry, 404 for a path that does not exist and names no chain,
// and 403 for a named file that cannot be opened.
func logTracePathError(e *logchain.SearchPathError) huma.StatusError {
	var outside *paths.ErrPathOutsideRoots
	var hidden *paths.ErrHiddenPath
	switch {
	case errors.As(e.Err, &outside):
		return NewSandboxError(outside)
	case errors.As(e.Err, &hidden):
		return ErrForbidden(e.Err.Error())
	case errors.Is(e.Err, fs.ErrNotExist):
		return ErrNotFound(fmt.Sprintf("Path not found: %s", e.Path))
	}
	return ErrFileAccess(e.Path, e.Err)
}
