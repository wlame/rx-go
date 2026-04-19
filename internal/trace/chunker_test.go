package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustWriteFile creates a tmp file with the given bytes and returns
// its absolute path. Registered for cleanup via t.Cleanup.
func mustWriteFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.log")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// ============================================================================
// Chunker boundary tests (the user-binding correctness surface)
// ============================================================================

func TestCreateFileTasks_EmptyFile(t *testing.T) {
	p := mustWriteFile(t, []byte{})
	tasks, err := CreateFileTasks(p)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task for empty file, got %d", len(tasks))
	}
	if tasks[0].Offset != 0 || tasks[0].Count != 0 {
		t.Errorf("offset/count = %d/%d, want 0/0", tasks[0].Offset, tasks[0].Count)
	}
}

func TestCreateFileTasks_SingleChunkForSmallFile(t *testing.T) {
	// 1 MB of 'a' chars. Below MIN_CHUNK_SIZE (default 20 MB) → 1 chunk.
	content := bytes1MB('a')
	p := mustWriteFile(t, content)
	tasks, err := CreateFileTasks(p)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("want 1 task, got %d", len(tasks))
	}
	if tasks[0].Offset != 0 {
		t.Errorf("offset = %d, want 0", tasks[0].Offset)
	}
	if tasks[0].Count != int64(len(content)) {
		t.Errorf("count = %d, want %d", tasks[0].Count, len(content))
	}
}

func TestCreateFileTasks_LastTaskRunsToEOF(t *testing.T) {
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	// Build a file whose size is 3.5 MB so chunk_size falls on a
	// non-round number — last task must still cover to EOF exactly.
	var content []byte
	for content = []byte{}; len(content) < 3*1024*1024+512*1024; {
		content = append(content, []byte("log line 0\n")...)
	}
	p := mustWriteFile(t, content)
	tasks, err := CreateFileTasks(p)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("want multi-chunk, got %d", len(tasks))
	}
	last := tasks[len(tasks)-1]
	if last.Offset+last.Count != int64(len(content)) {
		t.Errorf("last task end = %d, want %d", last.Offset+last.Count, len(content))
	}
}

func TestCreateFileTasks_OffsetsAreNewlineAligned(t *testing.T) {
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	// 3 MB of uniformly-newlined content. Every offset > 0 should
	// land on the byte AFTER a newline.
	var content []byte
	for i := 0; len(content) < 3*1024*1024; i++ {
		content = append(content, []byte("some log line text\n")...)
	}
	p := mustWriteFile(t, content)
	tasks, err := CreateFileTasks(p)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	if len(tasks) < 2 {
		t.Fatalf("want multi-chunk, got %d", len(tasks))
	}
	for i, task := range tasks {
		if task.Offset == 0 {
			continue
		}
		prev := content[task.Offset-1]
		if prev != '\n' {
			t.Errorf("task %d offset %d: byte before offset is %q, want '\\n'", i, task.Offset, prev)
		}
	}
}

func TestCreateFileTasks_CoversEntireFileWithoutOverlap(t *testing.T) {
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "1")
	t.Setenv("RX_MAX_SUBPROCESSES", "4")
	var content []byte
	for len(content) < 3*1024*1024 {
		content = append(content, []byte("line\n")...)
	}
	p := mustWriteFile(t, content)
	tasks, err := CreateFileTasks(p)
	if err != nil {
		t.Fatalf("CreateFileTasks: %v", err)
	}
	// Invariant: adjacent tasks tile without overlap.
	for i := 1; i < len(tasks); i++ {
		if tasks[i].Offset != tasks[i-1].EndOffset() {
			t.Errorf("task %d starts at %d, prev ends at %d", i, tasks[i].Offset, tasks[i-1].EndOffset())
		}
	}
	// Full coverage.
	if tasks[len(tasks)-1].EndOffset() != int64(len(content)) {
		t.Errorf("final end %d != file size %d", tasks[len(tasks)-1].EndOffset(), len(content))
	}
}

// findNextNewline behavioral tests

func TestFindNextNewline_FallsBackAtEOF(t *testing.T) {
	// File with no newline — findNextNewline should return end-of-read.
	content := strings.Repeat("x", 1000)
	p := mustWriteFile(t, []byte(content))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	got, err := findNextNewline(f, 0, 0)
	if err != nil {
		t.Fatalf("findNextNewline: %v", err)
	}
	if got != int64(len(content)) {
		t.Errorf("got %d, want %d (end-of-read)", got, len(content))
	}
}

func TestFindNextNewline_ReturnsPositionAfterNewline(t *testing.T) {
	content := "abc\ndef\nghi"
	p := mustWriteFile(t, []byte(content))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	// from offset 0 the first '\n' is at idx 3; we want 4 back.
	got, err := findNextNewline(f, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("first newline: got %d, want 4", got)
	}
	// From offset 5 (inside "def") the next '\n' is at idx 7; we want 8.
	got, err = findNextNewline(f, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Errorf("second newline: got %d, want 8", got)
	}
}

func TestFindNextNewline_PastEOF(t *testing.T) {
	content := "abc\n"
	p := mustWriteFile(t, []byte(content))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	got, err := findNextNewline(f, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Errorf("past EOF: got %d, want startOffset unchanged (10)", got)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// bytes1MB returns 1 MB of the given byte. Allocated once per test run;
// not shared to keep tests independent.
func bytes1MB(b byte) []byte {
	out := make([]byte, 1024*1024)
	for i := range out {
		out[i] = b
	}
	return out
}
