package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// treeInput is the huma-parsed query-string shape for GET /v1/tree.
// Pointer type on Path distinguishes "caller passed ?path=" (even empty)
// from "caller did not pass ?path=" which means "list search roots".
type treeInput struct {
	Path string `query:"path" example:"/var/log" doc:"Directory path to list. Omit to list search roots."`
}

// treeOutput wraps the TreeResponse body.
type treeOutput struct {
	Body rxtypes.TreeResponse
}

// registerTreeHandlers mounts GET /v1/tree.
//
// Matches rx-python/src/rx/web.py:1995-2133. Semantics:
//   - No path → list configured search roots (each as a TreeEntry).
//   - With path → validate within search roots, listdir it, attach
//     metadata per entry, sort (dirs first, then files, alphabetical).
//
// Returns 404 when the directory doesn't exist, 400 when it's a file
// (not a directory), 403 when outside the sandbox.
func registerTreeHandlers(_ *Server, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "tree",
		Method:      http.MethodGet,
		Path:        "/v1/tree",
		Summary:     "List directory contents",
		Description: "Lists files and directories within --search-root, with size/type/index metadata.",
		Tags:        []string{"FileTree"},
		Responses: errorResponses(api, http.StatusBadRequest, http.StatusForbidden,
			http.StatusNotFound),
	}, func(_ context.Context, in *treeInput) (*treeOutput, error) {
		// Empty path = "list search roots".
		if in.Path == "" {
			return &treeOutput{Body: listSearchRoots()}, nil
		}

		validated, err := paths.ValidatePathWithinRoots(in.Path)
		if err != nil {
			var perr *paths.ErrPathOutsideRoots
			if errors.As(err, &perr) {
				return nil, NewSandboxError(perr)
			}
			return nil, ErrForbidden(err.Error())
		}

		info, err := os.Stat(validated)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNotFound(fmt.Sprintf("Path not found: %s", validated))
			}
			return nil, ErrForbidden(fmt.Sprintf("Permission denied: %s", validated))
		}
		if !info.IsDir() {
			return nil, ErrBadRequest(fmt.Sprintf("Path is not a directory: %s", validated))
		}

		// The listing is read through the directory as it is pinned now,
		// so a directory swapped for a link after the check above is
		// refused rather than listed.
		dir, err := paths.Pin(validated)
		if err != nil {
			return nil, ErrForbidden(fmt.Sprintf("Permission denied: %s", validated))
		}
		entries, err := paths.ListDir(dir)
		if err != nil {
			return nil, ErrForbidden(fmt.Sprintf("Permission denied: %s", validated))
		}

		resp := buildTreeResponse(validated, entries)
		return &treeOutput{Body: resp}, nil
	})
}

// listSearchRoots builds a TreeResponse with one entry per configured
// search root. Parent is always nil and IsSearchRoot is always true.
// When no search roots are configured (unsandboxed CLI use) we return
// an empty list so the frontend can render a "no sandbox configured"
// message.
func listSearchRoots() rxtypes.TreeResponse {
	roots := paths.GetSearchRoots()
	if len(roots) == 0 {
		return rxtypes.TreeResponse{
			Path:         "/",
			Parent:       nil,
			IsSearchRoot: true,
			Entries:      []rxtypes.TreeEntry{},
			TotalEntries: 0,
		}
	}
	out := make([]rxtypes.TreeEntry, 0, len(roots))
	for _, root := range roots {
		pinned, err := paths.Pin(root)
		if err != nil {
			// A root that cannot be stated is still listed by name.
			out = append(out, rxtypes.TreeEntry{Name: filepath.Base(root), Path: root, Type: "directory"})
			continue
		}
		out = append(out, buildEntryMetadata(pinned, filepath.Base(root)))
	}
	return rxtypes.TreeResponse{
		Path:         "/",
		Parent:       nil,
		IsSearchRoot: true,
		Entries:      out,
		TotalEntries: len(out),
	}
}

// buildTreeResponse turns a directory listing into the full TreeResponse.
// Entries are sorted dirs-first (alphabetical), files-second (alphabetical)
// matching Python's sort order at rx-python/src/rx/web.py:2091-2102.
//
// A listing shows what a caller can open by name. paths.ListDir already
// leaves out hidden entries; a symlink it refused — out of every search
// root, into a hidden entry, or nowhere — is left out here, since
// listing it would only offer a link that returns 403 and show the size
// and type of a file outside the roots. A symlink to a directory inside
// the roots is listed as a directory.
func buildTreeResponse(absPath string, entries []paths.ListedEntry) rxtypes.TreeResponse {
	// Split dirs and files, sort each alphabetically (case-insensitive).
	dirs := make([]paths.ListedEntry, 0, len(entries))
	files := make([]paths.ListedEntry, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Refused != "":
			continue
		case e.IsDir():
			dirs = append(dirs, e)
		default:
			files = append(files, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	// Copy into a fresh slice so we don't mutate dirs (gocritic's
	// appendAssign check flags the alternate pattern). The new slice's
	// underlying array is owned by this function alone.
	merged := make([]paths.ListedEntry, 0, len(dirs)+len(files))
	merged = append(merged, dirs...)
	merged = append(merged, files...)

	// Determine parent & search-root status.
	snapshot := paths.GetSearchRoots()
	isSearchRoot := false
	for _, r := range snapshot {
		if absPath == r {
			isSearchRoot = true
			break
		}
	}
	var parent *string
	if !isSearchRoot {
		p := filepath.Dir(absPath)
		parent = &p
	}

	out := make([]rxtypes.TreeEntry, 0, len(merged))
	var totalSize int64
	for _, e := range merged {
		entry := buildEntryMetadata(e.Target, e.Name)
		out = append(out, entry)
		if entry.Size != nil {
			totalSize += *entry.Size
		}
	}

	resp := rxtypes.TreeResponse{
		Path:         absPath,
		Parent:       parent,
		IsSearchRoot: isSearchRoot,
		Entries:      out,
		TotalEntries: len(out),
	}
	if totalSize > 0 {
		resp.TotalSize = &totalSize
		human := output.HumanSize(totalSize)
		resp.TotalSizeHuman = &human
	}
	return resp
}

// buildEntryMetadata computes all TreeEntry fields for one entry, from
// the file or directory it was pinned to when it was listed. Every stat
// and read goes through the pin, so an entry retargeted since cannot
// lend it the size, type or children of a file elsewhere.
// Best-effort: if stat fails we return what we have; the frontend
// tolerates missing fields (they're all pointer types).
func buildEntryMetadata(target paths.Pinned, name string) rxtypes.TreeEntry {
	entryPath := target.Path()
	isDir := target.Info().IsDir()
	entry := rxtypes.TreeEntry{
		Name: name,
		Path: entryPath,
	}
	if isDir {
		entry.Type = "directory"
	} else {
		entry.Type = "file"
	}

	info, err := target.Stat()
	if err != nil {
		return entry // best-effort — still emit the name + path
	}
	mtime := formatWireTimestamp(info.ModTime())
	entry.ModifiedAt = &mtime

	if isDir {
		// Count children (non-recursive): what a listing of the
		// directory would show.
		if children, err := paths.ListDir(target); err == nil {
			count := 0
			for _, child := range children {
				if child.Refused == "" {
					count++
				}
			}
			entry.ChildrenCount = &count
		}
		return entry
	}

	// File metadata.
	size := info.Size()
	human := output.HumanSize(size)
	entry.Size = &size
	entry.SizeHuman = &human

	// What the file is, by the rule every command applies (filekind),
	// read through the pin: compressed or not by its bytes, and text or
	// not by the first 8 KiB of its text. A file the listing cannot open
	// is reported as neither compressed nor text.
	isCompressed, isText := false, false
	if kind, err := filekind.OfPinned(target); err == nil {
		isCompressed, isText = kind.IsCompressed(), kind.IsText()
		if isCompressed {
			name := kind.CompressionName()
			entry.CompressionFormat = &name
		}
	}
	entry.IsCompressed = &isCompressed
	entry.IsText = &isText

	// Index status. A peek, not a lookup: listing a directory does not
	// use its indexes, so it must not move the index cache metrics.
	if idx, err := index.PeekForSource(entryPath); err == nil && idx != nil {
		indexed := true
		entry.IsIndexed = &indexed
		if idx.LineCount != nil {
			entry.LineCount = idx.LineCount
		}
	} else {
		indexed := false
		entry.IsIndexed = &indexed
	}

	return entry
}

// wireTimestampLayout is RFC 3339 in UTC with exactly six fractional
// digits.
//
// Six because Python's datetime cannot hold nanoseconds, so rx-python
// could not match a finer rendering. Fixed rather than trimmed because
// Go's RFC3339Nano drops trailing zeros by design: a file whose mtime
// lands on a whole second rendered "…:24Z" while its neighbor rendered
// "…:24.438324Z", so two entries in one listing had different shapes.
const wireTimestampLayout = "2006-01-02T15:04:05.000000Z"

// formatWireTimestamp renders a time for a response body.
//
// Every timestamp that goes on the wire goes through here, so a new
// field cannot pick a different rendering. It is deliberately NOT used
// for the index's source_modified_at and created_at: those are cache
// format, owned by rx-python, and carry a naive local time that both
// backends already agree on — changing them would invalidate every
// index on disk.
func formatWireTimestamp(t time.Time) string {
	return t.UTC().Format(wireTimestampLayout)
}
