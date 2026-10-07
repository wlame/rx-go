package webapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"

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
// directory, which `rx logs list` gives from a terminal.
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
