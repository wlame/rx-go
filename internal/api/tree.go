package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wlame/rx/internal/security"
)

// TreeEntry represents a single file or directory in the tree listing.
type TreeEntry struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	IsDir    bool    `json:"is_dir"`
	Size     *int64  `json:"size"`     // nil for directories
	Modified *string `json:"modified"` // ISO 8601, nil on stat error
}

// TreeResponse is the response from GET /v1/tree.
type TreeResponse struct {
	Path    string      `json:"path"`
	Entries []TreeEntry `json:"entries"`
}

// handleTree handles GET /v1/tree — list directory contents within search roots.
//
// Query param: path (optional, defaults to first search root).
// Returns a flat list of entries (name, path, is_dir, size, modified).
// Path security is enforced via security.ValidatePath.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")

	// Default to the first search root when no path is provided.
	if dirPath == "" {
		if len(s.Config.SearchRoots) > 0 {
			dirPath = s.Config.SearchRoots[0]
		} else {
			// No search roots configured — use current working directory.
			cwd, err := os.Getwd()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "cannot determine working directory")
				return
			}
			dirPath = cwd
		}
	}

	// Validate path is within search roots (if configured).
	if len(s.Config.SearchRoots) > 0 {
		resolved, err := security.ValidatePath(dirPath, s.Config.SearchRoots)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		dirPath = resolved
	}

	// Verify the path exists and is a directory.
	info, err := os.Stat(dirPath)
	if os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("path not found: %s", dirPath))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("stat error: %v", err))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("path is not a directory: %s", dirPath))
		return
	}

	// Read directory entries.
	dirEntries, err := os.ReadDir(dirPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading directory: %v", err))
		return
	}

	entries := make([]TreeEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		entryPath := filepath.Join(dirPath, de.Name())
		entry := TreeEntry{
			Name:  de.Name(),
			Path:  entryPath,
			IsDir: de.IsDir(),
		}

		// Get file info for size and modification time.
		fi, err := de.Info()
		if err == nil {
			if !de.IsDir() {
				size := fi.Size()
				entry.Size = &size
			}
			modTime := fi.ModTime().UTC().Format(time.RFC3339)
			entry.Modified = &modTime
		}

		entries = append(entries, entry)
	}

	// Sort: directories first, then files, alphabetically within each group.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // directories first
		}
		return entries[i].Name < entries[j].Name
	})

	resp := TreeResponse{
		Path:    dirPath,
		Entries: entries,
	}

	writeJSON(w, http.StatusOK, resp)
}
