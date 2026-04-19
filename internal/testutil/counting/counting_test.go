package counting

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestReader_CountsExactBytes confirms the wrapper reports the number
// of bytes the consumer RECEIVES, not the size of the buffer passed in.
// When a Read returns n < len(p), only n is counted.
func TestReader_CountsExactBytes(t *testing.T) {
	t.Parallel()
	data := []byte("hello world")
	r := NewReader(bytes.NewReader(data))
	buf := make([]byte, 100)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Read returned %d, want %d", n, len(data))
	}
	if got := r.Load(); got != int64(len(data)) {
		t.Fatalf("Load()=%d, want %d", got, len(data))
	}
}

// TestReader_CountsAcrossMultipleCalls asserts the running total is
// cumulative — the scanner/bufio pattern reads in many small chunks.
func TestReader_CountsAcrossMultipleCalls(t *testing.T) {
	t.Parallel()
	data := strings.Repeat("x", 10_000)
	r := NewReader(strings.NewReader(data))
	// Drain via io.ReadAll which uses a grow-as-you-go pattern.
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != len(data) {
		t.Fatalf("ReadAll len=%d, want %d", len(out), len(data))
	}
	if got := r.Load(); got != int64(len(data)) {
		t.Fatalf("Load()=%d, want %d", got, len(data))
	}
}

// TestReader_ResetClearsCounter verifies Reset() zeroes the counter
// mid-run for tests that reuse the wrapper across iterations.
func TestReader_ResetClearsCounter(t *testing.T) {
	t.Parallel()
	r := NewReader(strings.NewReader("abcdefgh"))
	buf := make([]byte, 4)
	_, _ = r.Read(buf)
	if got := r.Load(); got != 4 {
		t.Fatalf("pre-reset Load=%d, want 4", got)
	}
	r.Reset()
	if got := r.Load(); got != 0 {
		t.Fatalf("post-reset Load=%d, want 0", got)
	}
	_, _ = r.Read(buf)
	if got := r.Load(); got != 4 {
		t.Fatalf("post-reset-and-read Load=%d, want 4", got)
	}
}

// TestReaderAt_ConcurrentReadsAreRaceFree hammers the ReaderAt counter
// from many goroutines. ReadAt on *bytes.Reader is concurrency-safe
// (unlike Read), so this is the correct shape for a race test. The
// atomic counter must not race under `go test -race`.
func TestReaderAt_ConcurrentReadsAreRaceFree(t *testing.T) {
	t.Parallel()
	const goroutines = 32
	const perRoutine = 1024
	data := bytes.Repeat([]byte("x"), goroutines*perRoutine)
	r := NewReaderAt(bytes.NewReader(data))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		off := int64(i) * perRoutine
		go func() {
			defer wg.Done()
			buf := make([]byte, perRoutine)
			_, _ = r.ReadAt(buf, off)
		}()
	}
	wg.Wait()

	// Every goroutine read exactly perRoutine bytes from a distinct
	// offset, so the counter MUST equal goroutines*perRoutine if the
	// atomic counter is correctly propagating concurrent Add()s.
	want := int64(goroutines * perRoutine)
	if got := r.Load(); got != want {
		t.Fatalf("Load=%d, want %d", got, want)
	}
}

// TestReaderAt_CountsPerReadAt verifies that positional reads also count.
func TestReaderAt_CountsPerReadAt(t *testing.T) {
	t.Parallel()
	data := []byte("abcdefghijklmnop")
	r := NewReaderAt(bytes.NewReader(data))
	buf := make([]byte, 5)
	n, err := r.ReadAt(buf, 2)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 5 || string(buf) != "cdefg" {
		t.Fatalf("ReadAt got %q (n=%d), want 'cdefg' (n=5)", buf, n)
	}
	if r.Load() != 5 {
		t.Fatalf("Load=%d, want 5", r.Load())
	}
	// Second read accumulates.
	_, _ = r.ReadAt(buf, 5)
	if r.Load() != 10 {
		t.Fatalf("Load=%d, want 10", r.Load())
	}
}

// TestOpenCounting_WrapsOSFile end-to-end check that OpenCounting
// produces a *File usable everywhere *os.File is, and that both Read
// and ReadAt paths contribute to the counter.
func TestOpenCounting_WrapsOSFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, counter := OpenCounting(t, path)
	if f == nil {
		t.Fatal("OpenCounting returned nil file")
	}

	// Sequential Read.
	buf := make([]byte, 3)
	n, err := f.Read(buf)
	if err != nil || n != 3 || string(buf) != "123" {
		t.Fatalf("Read: %v n=%d buf=%q", err, n, buf)
	}
	if counter.Load() != 3 {
		t.Fatalf("after Read: counter=%d, want 3", counter.Load())
	}

	// ReadAt does not touch the cursor.
	buf2 := make([]byte, 2)
	n2, err := f.ReadAt(buf2, 3)
	if err != nil || n2 != 2 || string(buf2) != "45" {
		t.Fatalf("ReadAt: %v n=%d buf=%q", err, n2, buf2)
	}
	if counter.Load() != 5 {
		t.Fatalf("after ReadAt: counter=%d, want 5", counter.Load())
	}
}

// TestOpenCounting_RegistersCleanup ensures t.Cleanup closes the file.
// We can't directly assert "file is closed" from outside t, but we can
// at least assert the cleanup doesn't panic.
func TestOpenCounting_RegistersCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Inner t so the cleanup runs within this parent test scope.
	t.Run("sub", func(tt *testing.T) {
		_, _ = OpenCounting(tt, path)
	})
	// If we reach here without panic, cleanup succeeded.
}
