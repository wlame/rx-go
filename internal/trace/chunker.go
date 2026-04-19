package trace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wlame/rx-go/internal/config"
)

// FileTask describes one chunk of a file — a work unit for the parallel
// scanner. Each FileTask covers a contiguous, newline-aligned byte
// range in the file.
//
// Byte semantics (critical to match Python):
//
//   - Offset is the INCLUSIVE start byte. Guaranteed to be 0 for task
//     0, or the byte immediately AFTER a newline for all other tasks.
//   - Count is the number of bytes in this chunk. Offset+Count is the
//     EXCLUSIVE end byte; the next task (if any) starts exactly there.
//   - For the last task, Offset+Count equals the file size.
//
// The whole file is covered by a contiguous sequence of tasks with no
// overlap — ripgrep itself handles the line-boundary stitching because
// every task starts on a newline-aligned offset and runs to the byte
// before the next task's start.
//
// Parity: this is identical to rx-python/src/rx/file_utils.py::FileTask.
type FileTask struct {
	TaskID   int    // zero-based index
	FilePath string // absolute path recommended
	Offset   int64  // inclusive start byte (aligned to newline for TaskID>0)
	Count    int64  // byte count
}

// EndOffset is the exclusive end byte for this task.
func (t FileTask) EndOffset() int64 { return t.Offset + t.Count }

// ============================================================================
// Chunker
// ============================================================================

// findNextNewlineBytes reads up to the provided amount from r (which
// should be positioned at startOffset already), looking for the first
// '\n'. If no newline is found within the read window, returns the
// position at end-of-read as a best-effort boundary.
//
// Returns the ABSOLUTE offset of the byte AFTER the newline (i.e. the
// start of the next line).
//
// Parity with Python's find_next_newline (rx-python/src/rx/file_utils.py):
//
//	Python reads up to 256 KB forward from `offset`, finds the first
//	'\n', and returns `offset + pos + 1`. If no newline found within
//	the window, returns the end of the read window.
//
// The Go version does the same, except it uses os.File.ReadAt instead
// of seek+read so there's no shared file cursor — this is the native
// chunking path (Decision 5.1).
func findNextNewline(f *os.File, startOffset int64, maxReadBytes int) (int64, error) {
	if maxReadBytes <= 0 {
		// Match Python's default of 256 KB.
		maxReadBytes = 256 * 1024
	}
	// Cap the read at the available tail of the file — avoids io.EOF
	// surprises when the requested offset is near the end.
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	remaining := fi.Size() - startOffset
	if remaining <= 0 {
		// At or past EOF — no newline to find; return the start offset.
		return startOffset, nil
	}
	if int64(maxReadBytes) > remaining {
		maxReadBytes = int(remaining)
	}

	buf := make([]byte, maxReadBytes)
	n, err := f.ReadAt(buf, startOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("findNextNewline: ReadAt at %d: %w", startOffset, err)
	}
	idx := bytes.IndexByte(buf[:n], '\n')
	if idx < 0 {
		// No newline within the read window. Python returns the end of
		// the window — we do the same. The caller treats this as a
		// "best-effort" split point; if the file is a single massive
		// line, the chunker degrades to one task anyway.
		return startOffset + int64(n), nil
	}
	// Python returns `offset + pos + 1` — the position AFTER the
	// newline. That way task N+1 starts at the first byte of the
	// NEXT line, not on the newline itself.
	return startOffset + int64(idx) + 1, nil
}

// GetFileOffsets computes the list of starting byte offsets for chunk
// tasks on a file. Offsets are newline-aligned (except offset 0).
//
// Algorithm (matches rx-python/src/rx/file_utils.py::get_file_offsets):
//
//  1. Estimate the maximum number of chunks the file fits given
//     MinChunkSize: `max_chunks_by_size = file_size / MinChunkSize`.
//  2. Cap by RX_MAX_SUBPROCESSES (default 20).
//  3. Floor at 1.
//  4. Compute raw offsets at `i * chunk_size` for i in [0, num_chunks).
//  5. Align offsets >= 1 to the next newline boundary.
//  6. Return the aligned offsets. Offset 0 is always first.
//
// The returned slice has between 1 and MAX_SUBPROCESSES entries, and
// is strictly monotonically increasing (enforced by dedup at the end).
// If two raw offsets collapse to the same aligned offset (possible
// near long-line boundaries), the duplicates are pruned silently.
func GetFileOffsets(path string, fileSize int64) ([]int64, error) {
	if fileSize <= 0 {
		// Empty file — one task covering zero bytes, matches Python's
		// behavior: `[FileTask(0, path, 0, 0)]`.
		return []int64{0}, nil
	}

	minChunkSize := int64(config.MinChunkSizeMB()) * 1024 * 1024
	if minChunkSize <= 0 {
		// Defend against RX_MIN_CHUNK_SIZE_MB=0: fall back to default.
		minChunkSize = int64(config.DefaultMinChunkSizeMB) * 1024 * 1024
	}
	maxSubs := int64(config.MaxSubprocesses())
	if maxSubs < 1 {
		maxSubs = 1
	}

	maxChunksBySize := fileSize / minChunkSize
	numChunks := maxChunksBySize
	if numChunks > maxSubs {
		numChunks = maxSubs
	}
	if numChunks < 1 {
		numChunks = 1
	}

	// Parity detail: Python's `chunk_size = file_size // num_chunks`
	// uses integer division. Go's int64 division rounds toward zero
	// for positive values, matching Python's floor division on
	// positive values.
	chunkSize := fileSize / numChunks

	if numChunks == 1 {
		// Single chunk — no alignment work needed.
		return []int64{0}, nil
	}

	// Open the file once and reuse the handle for all alignment lookups.
	// Python opens a fresh handle inside find_next_newline for each
	// offset; that's wasteful. ReadAt is goroutine-safe so we could
	// parallelise alignment, but N <= 20 makes it pointless.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("GetFileOffsets: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	aligned := make([]int64, 0, numChunks)
	aligned = append(aligned, 0)
	for i := int64(1); i < numChunks; i++ {
		raw := i * chunkSize
		al, err := findNextNewline(f, raw, 0)
		if err != nil {
			return nil, err
		}
		// Prune duplicates: if alignment collapsed onto a previous
		// offset (because the region had no newlines), skip this chunk.
		// Same task count invariant as Python when we end up with fewer
		// chunks than originally planned.
		if al > aligned[len(aligned)-1] && al < fileSize {
			aligned = append(aligned, al)
		}
	}

	return aligned, nil
}

// CreateFileTasks splits a file into FileTasks by:
//  1. Calling GetFileOffsets to get the newline-aligned chunk starts.
//  2. Computing per-task byte counts (difference between adjacent
//     offsets; last task runs to end-of-file).
//
// Caller must ensure `path` is a regular file — directories and special
// files are rejected upstream in the engine.
func CreateFileTasks(path string) ([]FileTask, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("CreateFileTasks: stat %s: %w", path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("CreateFileTasks: %s is a directory", path)
	}
	fileSize := fi.Size()

	offsets, err := GetFileOffsets(path, fileSize)
	if err != nil {
		return nil, err
	}

	tasks := make([]FileTask, len(offsets))
	for i, off := range offsets {
		var count int64
		if i == len(offsets)-1 {
			count = fileSize - off
		} else {
			count = offsets[i+1] - off
		}
		tasks[i] = FileTask{
			TaskID:   i,
			FilePath: path,
			Offset:   off,
			Count:    count,
		}
	}
	return tasks, nil
}
