package trace

// Task represents a chunk of work to be processed
type Task struct {
	ID       string // Unique identifier for this task
	FilePath string // Path to the file
	Offset   int64  // Byte offset to start reading
	Length   int64  // Number of bytes to read
	ChunkID  int    // Chunk index for this file
}

// Result represents the result of processing a task
type Result struct {
	TaskID       string
	FilePath     string
	Matches      []MatchResult
	Error        error
	ChunkID      int
}

// MatchResult represents a single match found during processing
type MatchResult struct {
	Offset         int64  // Absolute byte offset in file
	LineText       string // Content of the matched line
	LineNumber     int    // Relative line number within chunk
	PatternMatched string // Which pattern matched (for multi-pattern searches)
}
