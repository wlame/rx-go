// Package counting provides I/O wrappers that record the total number
// of bytes read, for use in byte-budget tests that assert a code path
// does NOT read more of a file than its contract allows.
//
// This is test-only infrastructure. It lives under internal/testutil so
// production binaries never depend on it, and it MUST NOT be imported
// from non-test code.
//
// # Why byte-budget tests?
//
// Correctness tests verify WHAT a function returns. Byte-budget tests
// verify HOW MUCH it reads to produce that result. Regressions like
// "break condition is unreachable so the loop reads to EOF" pass every
// correctness assertion — the output is right, it just takes 3000× as
// long. Byte-budget tests catch the performance contract at unit-test
// time, not during end-user profiling.
//
// # Typical use
//
//	f, counter := counting.OpenCounting(t, "/tmp/fixture.txt")
//	defer f.Close()
//
//	result := codeUnderTest(f) // function uses io.ReaderAt
//
//	// Correctness:
//	require.Equal(t, expected, result)
//	// Budget:
//	require.Less(t, counter.Load(), int64(maxBudget),
//	    "read %d bytes, budget was %d", counter.Load(), maxBudget)
package counting

import (
	"io"
	"os"
	"sync/atomic"
	"testing"
)

// Reader wraps an io.Reader and atomically records the running total
// of bytes returned by Read. Concurrency-safe: multiple goroutines may
// call Read, but Load() returns a correct snapshot at any moment.
//
// We use an atomic counter because the trace engine reads files from
// multiple goroutines (one per chunk). A plain int would race under -race.
type Reader struct {
	inner io.Reader
	count atomic.Int64
}

// NewReader wraps r with a byte counter. The zero value of the counter
// is 0 — a fresh wrapper has read nothing yet.
func NewReader(r io.Reader) *Reader {
	return &Reader{inner: r}
}

// Read implements io.Reader. Records the number of bytes returned
// (not the size of p) — zero reads and EOF-with-partial both track
// exactly what the consumer actually received.
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.count.Add(int64(n))
	}
	return n, err
}

// Load returns the current byte count. Safe to call from any goroutine.
func (r *Reader) Load() int64 {
	return r.count.Load()
}

// Reset zeroes the counter. Useful in tests that exercise a function
// multiple times and want per-call budgets.
func (r *Reader) Reset() {
	r.count.Store(0)
}

// ReaderAt wraps an io.ReaderAt and atomically records the total bytes
// returned from ReadAt. This is the random-access equivalent of Reader.
// The samples/trace code paths use ReadAt (via os.File or
// io.SectionReader) so tests that want to assert byte budgets on those
// paths need this wrapper.
type ReaderAt struct {
	inner io.ReaderAt
	count atomic.Int64
}

// NewReaderAt wraps r with a byte counter.
func NewReaderAt(r io.ReaderAt) *ReaderAt {
	return &ReaderAt{inner: r}
}

// ReadAt implements io.ReaderAt. Same counting semantics as Reader.Read.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.inner.ReadAt(p, off)
	if n > 0 {
		r.count.Add(int64(n))
	}
	return n, err
}

// Load returns the current byte count. Safe to call from any goroutine.
func (r *ReaderAt) Load() int64 {
	return r.count.Load()
}

// Reset zeroes the counter.
func (r *ReaderAt) Reset() {
	r.count.Store(0)
}

// File is a testing helper that wraps *os.File and counts bytes for both
// Read and ReadAt access patterns. Most production code reaches files
// via os.Open() which returns *os.File; tests that want to observe the
// byte traffic can use OpenCounting() to get a drop-in replacement.
//
// Note: File deliberately does NOT implement io.Seeker or Close via a
// wrapper. Callers get the underlying *os.File via Unwrap() for seeking
// and MUST call Close() on the underlying file themselves (or use
// t.Cleanup to wire it up). Keeping the wrapper narrow avoids accidentally
// hiding state from the system under test.
type File struct {
	*os.File
	counter *atomic.Int64
}

// Read overrides *os.File.Read to count bytes.
func (f *File) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.counter.Add(int64(n))
	}
	return n, err
}

// ReadAt overrides *os.File.ReadAt to count bytes.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	if n > 0 {
		f.counter.Add(int64(n))
	}
	return n, err
}

// OpenCounting opens path for reading and returns a File wrapper along
// with a pointer to the byte counter. The file is registered with
// t.Cleanup so callers don't need to remember to Close.
//
// Returns (nil, nil) if the test has already failed via require.NoError
// style fatal checks — by calling t.Fatalf here we guarantee the test
// halts before any code under test runs against a nil file.
//
// Example:
//
//	f, counter := counting.OpenCounting(t, fixturePath)
//	// ... call code that accepts *os.File ...
//	assert.Less(t, counter.Load(), int64(1<<20), "should read <1 MiB")
func OpenCounting(t *testing.T, path string) (*File, *atomic.Int64) {
	t.Helper()
	raw, err := os.Open(path)
	if err != nil {
		t.Fatalf("counting.OpenCounting(%q): %v", path, err)
		return nil, nil
	}
	counter := new(atomic.Int64)
	t.Cleanup(func() {
		_ = raw.Close()
	})
	return &File{File: raw, counter: counter}, counter
}
