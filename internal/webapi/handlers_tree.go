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

	"github.com/wlame/rx-go/internal/compression"
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

		entries, err := os.ReadDir(validated)
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
		entry := buildEntryMetadata(root, filepath.Base(root), true)
		out = append(out, entry)
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
func buildTreeResponse(absPath string, entries []os.DirEntry) rxtypes.TreeResponse {
	// Split dirs and files, sort each alphabetically (case-insensitive).
	dirs := make([]os.DirEntry, 0, len(entries))
	files := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name()) < strings.ToLower(dirs[j].Name())
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name()) < strings.ToLower(files[j].Name())
	})

	// Copy into a fresh slice so we don't mutate dirs (gocritic's
	// appendAssign check flags the alternate pattern). The new slice's
	// underlying array is owned by this function alone.
	merged := make([]os.DirEntry, 0, len(dirs)+len(files))
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
		entry := buildEntryMetadata(filepath.Join(absPath, e.Name()), e.Name(), e.IsDir())
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

// buildEntryMetadata computes all TreeEntry fields for a single path.
// Best-effort: if stat fails we return what we have; the frontend
// tolerates missing fields (they're all pointer types).
func buildEntryMetadata(entryPath, name string, isDir bool) rxtypes.TreeEntry {
	entry := rxtypes.TreeEntry{
		Name: name,
		Path: entryPath,
	}
	if isDir {
		entry.Type = "directory"
	} else {
		entry.Type = "file"
	}

	info, err := os.Stat(entryPath)
	if err != nil {
		return entry // best-effort — still emit the name + path
	}
	mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
	entry.ModifiedAt = &mtime

	if isDir {
		// Count children (non-recursive).
		if children, err := os.ReadDir(entryPath); err == nil {
			count := len(children)
			entry.ChildrenCount = &count
		}
		return entry
	}

	// File metadata.
	size := info.Size()
	human := output.HumanSize(size)
	entry.Size = &size
	entry.SizeHuman = &human

	// Compression detection.
	if compression.IsCompressed(entryPath) {
		t := true
		entry.IsCompressed = &t
		cformat, _ := compression.DetectFromPath(entryPath)
		if cformat != compression.FormatNone {
			fs := string(cformat)
			entry.CompressionFormat = &fs
		}
		// Compressed files are "text" for our purposes.
		entry.IsText = &t
	} else {
		f := false
		entry.IsCompressed = &f
		isText := looksLikeTextFile(entryPath)
		entry.IsText = &isText
	}

	// Index status.
	if idx, err := index.LoadForSource(entryPath); err == nil && idx != nil {
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

// looksLikeTextFile returns true if the first 512 bytes of path contain
// no NUL bytes. Same heuristic as internal/trace/engine.go:isTextFile.
// Kept private here to avoid a circular import.
func looksLikeTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return false
		}
	}
	return true
}
