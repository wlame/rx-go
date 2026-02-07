package models

// TreeRequest represents a request to list files in a directory
type TreeRequest struct {
	Path      string `json:"path" validate:"required"`
	Recursive bool   `json:"recursive,omitempty"`
	MaxDepth  int    `json:"max_depth,omitempty"`
}

// TreeResponse represents the response from a tree operation
type TreeResponse struct {
	Path    string     `json:"path"`
	Files   []FileInfo `json:"files"`
	Directories []string `json:"directories,omitempty"`
}

// FileInfo represents information about a file
type FileInfo struct {
	Path         string `json:"path"`
	SizeBytes    int64  `json:"size_bytes"`
	ModifiedAt   string `json:"modified_at"` // RFC3339 format
	IsCompressed bool   `json:"is_compressed"`
	FileType     FileType `json:"file_type"`
}
