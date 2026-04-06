package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wlame/rx/internal/fileutil"
	"github.com/wlame/rx/internal/index"
	"github.com/wlame/rx/internal/security"
)

// TreeEntry represents a single file or directory in the tree listing.
// Matches the Python TreeEntry model exactly for frontend compatibility.
type TreeEntry struct {
	Name              string  `json:"name"`
	Path              string  `json:"path"`
	Type              string  `json:"type"` // "file" or "directory"
	Size              *int64  `json:"size"`
	SizeHuman         *string `json:"size_human"`
	ModifiedAt        *string `json:"modified_at"`
	IsText            *bool   `json:"is_text"`
	IsCompressed      *bool   `json:"is_compressed"`
	CompressionFormat *string `json:"compression_format"`
	IsIndexed         *bool   `json:"is_indexed"`
	LineCount         *int    `json:"line_count"`
	ChildrenCount     *int    `json:"children_count"`
}

// TreeResponse is the response from GET /v1/tree.
// Matches the Python TreeResponse model exactly for frontend compatibility.
type TreeResponse struct {
	Path           string      `json:"path"`
	Parent         *string     `json:"parent"`
	IsSearchRoot   bool        `json:"is_search_root"`
	Entries        []TreeEntry `json:"entries"`
	TotalEntries   int         `json:"total_entries"`
	TotalSize      *int64      `json:"total_size"`
	TotalSizeHuman *string     `json:"total_size_human"`
}

// handleTree handles GET /v1/tree — list directory contents within search roots.
//
// When no path is provided, returns the list of search roots (matching Python behavior).
// When a path is provided, lists directory contents with full metadata per entry.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")

	// If no path provided, return search roots listing.
	if dirPath == "" {
		s.handleTreeRoots(w)
		return
	}

	// Validate path is within search roots.
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
		writeError(w, http.StatusNotFound, fmt.Sprintf("Path not found: %s", dirPath))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("stat error: %v", err))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Path is not a directory: %s", dirPath))
		return
	}

	// Read directory entries.
	dirEntries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsPermission(err) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("Permission denied: %s", dirPath))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("error reading directory: %v", err))
		return
	}

	// Classify entries into dirs and files, then sort: dirs first, alphabetical.
	type classified struct {
		name    string
		path    string
		isDir   bool
		sortKey string // lowercased name for case-insensitive sort
	}
	var dirs, files []classified

	for _, de := range dirEntries {
		entryPath := filepath.Join(dirPath, de.Name())
		c := classified{
			name:    de.Name(),
			path:    entryPath,
			isDir:   de.IsDir(),
			sortKey: strings.ToLower(de.Name()),
		}
		if de.IsDir() {
			dirs = append(dirs, c)
		} else {
			files = append(files, c)
		}
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i].sortKey < dirs[j].sortKey })
	sort.Slice(files, func(i, j int) bool { return files[i].sortKey < files[j].sortKey })

	sorted := append(dirs, files...)

	// Build entries with full metadata.
	entries := make([]TreeEntry, 0, len(sorted))
	var totalSize int64

	for _, c := range sorted {
		entry := buildTreeEntry(c.path, c.name, c.isDir, s.Config.CacheDir)
		entries = append(entries, entry)
		if entry.Size != nil {
			totalSize += *entry.Size
		}
	}

	// Determine parent path and whether this is a search root.
	isSearchRoot := false
	for _, root := range s.Config.SearchRoots {
		absRoot, _ := filepath.Abs(root)
		absDir, _ := filepath.Abs(dirPath)
		if absRoot == absDir {
			isSearchRoot = true
			break
		}
	}

	var parent *string
	if !isSearchRoot {
		p := filepath.Dir(dirPath)
		parent = &p
	}

	var totalSizePtr *int64
	var totalSizeHuman *string
	if totalSize > 0 {
		totalSizePtr = &totalSize
		h := humanReadableSize(totalSize)
		totalSizeHuman = &h
	}

	resp := TreeResponse{
		Path:           dirPath,
		Parent:         parent,
		IsSearchRoot:   isSearchRoot,
		Entries:        entries,
		TotalEntries:   len(entries),
		TotalSize:      totalSizePtr,
		TotalSizeHuman: totalSizeHuman,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleTreeRoots returns the search roots listing when no path is given.
// Matches Python's behavior: path="/", is_search_root=true, entries=search roots.
func (s *Server) handleTreeRoots(w http.ResponseWriter) {
	entries := make([]TreeEntry, 0, len(s.Config.SearchRoots))

	for _, root := range s.Config.SearchRoots {
		name := filepath.Base(root)
		entry := buildTreeEntry(root, name, true, s.Config.CacheDir)
		entries = append(entries, entry)
	}

	resp := TreeResponse{
		Path:         "/",
		Parent:       nil,
		IsSearchRoot: true,
		Entries:      entries,
		TotalEntries: len(entries),
	}

	writeJSON(w, http.StatusOK, resp)
}

// buildTreeEntry creates a TreeEntry with full metadata for a file or directory.
// Matches Python's get_entry_metadata function.
func buildTreeEntry(entryPath, name string, isDir bool, cacheDir string) TreeEntry {
	entry := TreeEntry{
		Name: name,
		Path: entryPath,
	}

	if isDir {
		entry.Type = "directory"
	} else {
		entry.Type = "file"
	}

	stat, err := os.Stat(entryPath)
	if err != nil {
		return entry
	}

	modTime := stat.ModTime().Format(time.RFC3339)
	entry.ModifiedAt = &modTime

	if isDir {
		// Count direct children.
		children, err := os.ReadDir(entryPath)
		if err == nil {
			count := len(children)
			entry.ChildrenCount = &count
		}
	} else {
		// File-specific metadata.
		size := stat.Size()
		entry.Size = &size
		h := humanReadableSize(size)
		entry.SizeHuman = &h

		// Classify file in a single header read instead of separate Detect + IsTextFile
		// calls, which would each open and read the file independently.
		fi := fileutil.ClassifyFile(entryPath, size)
		switch fi.Classification {
		case fileutil.ClassCompressed:
			isComp := true
			entry.IsCompressed = &isComp
			fmtStr := string(fi.CompressionFormat)
			entry.CompressionFormat = &fmtStr
			isText := true
			entry.IsText = &isText
		default:
			isComp := false
			entry.IsCompressed = &isComp
			isText := fi.Classification == fileutil.ClassText
			entry.IsText = &isText
		}

		// Check index status.
		if cacheDir != "" {
			cachePath := index.IndexCachePath(cacheDir, entryPath)
			idx, loadErr := index.Load(cachePath)
			if loadErr == nil && idx != nil && index.Validate(idx, entryPath) {
				isIndexed := true
				entry.IsIndexed = &isIndexed
				if lc := idx.GetLineCount(); lc != nil {
					entry.LineCount = lc
				}
			} else {
				isIndexed := false
				entry.IsIndexed = &isIndexed
			}
		} else {
			isIndexed := false
			entry.IsIndexed = &isIndexed
		}
	}

	return entry
}

// humanReadableSize formats bytes into a human-readable string.
func humanReadableSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case bytes >= tb:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(tb))
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
