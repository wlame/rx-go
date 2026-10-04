package trace

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/wlame/rx-go/internal/config"
	sandbox "github.com/wlame/rx-go/internal/paths"
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
	TaskID int // zero-based index
	// Source is the file, pinned to the file that was checked: the
	// worker reads it through Source.Open, which refuses a path that
	// leads elsewhere by then.
	Source sandbox.Pinned
	Offset int64 // inclusive start byte (aligned to newline for TaskID>0)
	Count  int64 // byte count
}

// EndOffset is the exclusive end byte for this task.
func (t FileTask) EndOffset() int64 { return t.Offset + t.Count }

// ============================================================================
// Chunker
// ============================================================================

// newlineSearchReadBytes is the size of one read of the chunk-boundary
// newline search. A line longer than this costs the search more reads
// of the same buffer, never more memory.
const newlineSearchReadBytes = 256 * 1024

// findNextNewline returns the start of the line after the first '\n' at
// or after from: the absolute offset of the byte after that newline. It
// reads r forward from from, one len(buf) read at a time, until it
// finds a newline or reaches limit, and returns limit when no newline
// lies in [from, limit). It never reads past limit.
//
// A file shorter than limit (truncated after its size was taken) ends
// the search at its end, which also returns limit: the scan of the
// chunks then fails on its own read, rather than this loop spinning on
// empty reads.
//
// r is an io.ReaderAt (the *os.File the pin opened, in production), so
// the search moves no shared file cursor.
func findNextNewline(r io.ReaderAt, from, limit int64, buf []byte) (int64, error) {
	for pos := from; pos < limit; {
		window := buf[:min(int64(len(buf)), limit-pos)]
		n, err := r.ReadAt(window, pos)
		// ReadAt may return bytes together with io.EOF, so look at the
		// bytes before the error.
		if idx := bytes.IndexByte(window[:n], '\n'); idx >= 0 {
			// The position AFTER the newline: the next chunk starts at
			// the first byte of the next line, not on the newline.
			return pos + int64(idx) + 1, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("findNextNewline: ReadAt at %d: %w", pos, err)
		}
		if n == 0 {
			break // the file ends before limit
		}
		pos += int64(n)
	}
	return limit, nil
}

// chunkStarts returns the first byte of each of up to numChunks chunks
// of the first fileSize bytes of r. The first start is 0; every other
// is the first byte of a line, found by searching forward from the
// tentative boundary i*fileSize/numChunks for the next newline. The
// starts strictly increase and stay below fileSize, so the chunks tile
// the file without overlap and without cutting a line.
//
// A line longer than the gap between tentative boundaries swallows the
// boundaries it covers: the search from the first of them runs to the
// line's end, and the others collapse onto that same start and are
// dropped. A file of very long lines therefore gets fewer, larger
// chunks — less parallelism, never a different answer.
//
// INVARIANT (bounded reads): every search covers bytes no earlier
// search covered, because a tentative boundary that lies before the
// point the previous search reached is skipped without reading (its
// next line start is that point). The planning therefore reads each byte
// from the first tentative boundary to the end of the file at most
// once, plus at most one read of newlineSearchReadBytes past the newline
// for each boundary, with a single buffer of that size: a 100 GB file
// that is one line is read once here, not once per boundary.
func chunkStarts(r io.ReaderAt, fileSize, numChunks int64) ([]int64, error) {
	chunkSize := fileSize / numChunks
	buf := make([]byte, newlineSearchReadBytes)
	starts := make([]int64, 0, numChunks)
	starts = append(starts, 0)
	// searchedTo is where the previous search ended: the start of the
	// line after the newline it found, or fileSize when it found none.
	// No newline lies between that search's tentative boundary and
	// searchedTo-1.
	searchedTo := int64(0)
	for i := int64(1); i < numChunks; i++ {
		raw := i * chunkSize
		if raw < searchedTo {
			// The previous search already read past raw without a
			// newline: the next line start after raw is searchedTo,
			// which is a chunk start already, or the end of the file.
			continue
		}
		next, err := findNextNewline(r, raw, fileSize, buf)
		if err != nil {
			return nil, err
		}
		searchedTo = next
		// The end of the file starts no chunk: the last chunk runs to it.
		if next > starts[len(starts)-1] && next < fileSize {
			starts = append(starts, next)
		}
	}
	return starts, nil
}

// GetFileOffsets computes the list of starting byte offsets for chunk
// tasks on a file. Offsets are newline-aligned (except offset 0).
//
// Algorithm (rx-python/src/rx/file_utils.py::get_file_offsets, except
// for step 5):
//
//  1. Estimate the maximum number of chunks the file fits given
//     MinChunkSize: `max_chunks_by_size = file_size / MinChunkSize`.
//  2. Cap by RX_MAX_SUBPROCESSES (default 20).
//  3. Floor at 1.
//  4. Compute raw offsets at `i * chunk_size` for i in [0, num_chunks),
//     chunk_size by integer division as Python's `//`.
//  5. Move each offset >= 1 to the start of the next line, however far
//     ahead it is (chunkStarts). rx-python looks only 256 KiB ahead and
//     otherwise cuts inside the line.
//  6. Return the aligned offsets. Offset 0 is always first.
//
// The returned slice has between 1 and MAX_SUBPROCESSES entries, and
// is strictly monotonically increasing. Raw offsets that land in the
// same long line collapse onto one start, so a file of long lines gets
// fewer chunks than planned.
func GetFileOffsets(src sandbox.Pinned, fileSize int64) ([]int64, error) {
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

	if numChunks == 1 {
		// Single chunk — no alignment work needed.
		return []int64{0}, nil
	}

	// Open the file once, through its pin, and reuse the handle for
	// every boundary search. Source.Open refuses a path that no longer
	// leads to the file the trace checked.
	f, err := src.Open()
	if err != nil {
		return nil, fmt.Errorf("GetFileOffsets: open %s: %w", src.Path(), err)
	}
	defer func() { _ = f.Close() }()

	return chunkStarts(f, fileSize, numChunks)
}

// CreateFileTasks splits a file into FileTasks by:
//  1. Calling GetFileOffsets to get the newline-aligned chunk starts.
//  2. Computing per-task byte counts (difference between adjacent
//     offsets; last task runs to end-of-file).
//
// The path is pinned (sandbox.Pin) first, which checks it as a named
// path; the tasks then read only the file it led to.
func CreateFileTasks(path string) ([]FileTask, error) {
	src, err := sandbox.Pin(path)
	if err != nil {
		return nil, fmt.Errorf("CreateFileTasks: %w", err)
	}
	if src.Info().IsDir() {
		return nil, fmt.Errorf("CreateFileTasks: %s is a directory", path)
	}
	return planFileTasks(src, src.Info().Size())
}

// planFileTasks splits the first fileSize bytes of src into FileTasks.
// The size is the caller's: the engine plans from the stat it records
// as the file's identity, so the tasks, and therefore the scan, cover
// exactly the bytes that identity describes even if the file grows
// while they run.
func planFileTasks(src sandbox.Pinned, fileSize int64) ([]FileTask, error) {
	offsets, err := GetFileOffsets(src, fileSize)
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
			TaskID: i,
			Source: src,
			Offset: off,
			Count:  count,
		}
	}
	return tasks, nil
}
