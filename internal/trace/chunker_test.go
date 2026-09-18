package trace

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/counting"
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

func TestFindNextNewline_ReturnsTheLimitWhenNoNewlineFollows(t *testing.T) {
	content := strings.Repeat("x", 1000)
	buf := make([]byte, 64)
	got, err := findNextNewline(strings.NewReader(content), 0, int64(len(content)), buf)
	if err != nil {
		t.Fatalf("findNextNewline: %v", err)
	}
	if got != int64(len(content)) {
		t.Errorf("got %d, want %d (the limit)", got, len(content))
	}
}

func TestFindNextNewline_ReturnsPositionAfterNewline(t *testing.T) {
	content := "abc\ndef\nghi"
	r := strings.NewReader(content)
	buf := make([]byte, 64)
	// from offset 0 the first '\n' is at idx 3; we want 4 back.
	got, err := findNextNewline(r, 0, int64(len(content)), buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("first newline: got %d, want 4", got)
	}
	// From offset 5 (inside "def") the next '\n' is at idx 7; we want 8.
	got, err = findNextNewline(r, 5, int64(len(content)), buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Errorf("second newline: got %d, want 8", got)
	}
}

// A newline further away than one read is still found: the search reads
// on, one buffer at a time, instead of stopping inside the line.
func TestFindNextNewline_ReadsPastOneBufferToTheNewline(t *testing.T) {
	content := strings.Repeat("x", 1000) + "\nnext"
	got, err := findNextNewline(strings.NewReader(content), 10, int64(len(content)), make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1001 {
		t.Errorf("got %d, want 1001 (the byte after the newline)", got)
	}
}

func TestFindNextNewline_StopsAtTheLimit(t *testing.T) {
	// The newline at byte 100 lies past the limit of 50: the search
	// covers only the bytes it was asked about.
	content := strings.Repeat("x", 100) + "\n"
	got, err := findNextNewline(strings.NewReader(content), 0, 50, make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Errorf("got %d, want the limit 50", got)
	}
}

func TestFindNextNewline_FileShorterThanTheLimit(t *testing.T) {
	// A file truncated after its size was taken ends the search at the
	// limit instead of reading for ever.
	got, err := findNextNewline(strings.NewReader("abc"), 0, 100, make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Errorf("got %d, want the limit 100", got)
	}
}

// longLineText returns lines of "short line\n" up to about size bytes,
// with one line of length bytes (newline included) starting at each of
// the given offsets.
func longLineText(size int, longs map[int]int) []byte {
	var b bytes.Buffer
	for b.Len() < size {
		if length, ok := longs[b.Len()]; ok {
			b.WriteString(strings.Repeat("x", length-1))
			b.WriteByte('\n')
			continue
		}
		b.WriteString("short line\n") // 11 bytes; long lines start on multiples of 11
	}
	return b.Bytes()
}

// Every chunk start is the first byte of a line, whatever the length of
// the line across a tentative boundary: a line longer than the newline
// search's first read moves the boundary to its end, and a line longer
// than a chunk merges the chunks it covers.
func TestChunkStarts_AreLineStarts(t *testing.T) {
	const (
		mib = 1 << 20
		// longLine is longer than one read of the newline search, and a
		// multiple of the short line's 11 bytes, so the lines after it
		// still start where longLineText looks for the next long line.
		longLine = 11 * 56000
	)
	cases := []struct {
		name      string
		text      []byte
		numChunks int64
		want      int // number of chunk starts
	}{
		{"ordinary lines", longLineText(4*mib, nil), 4, 4},
		{"a long line across one boundary", longLineText(4*mib, map[int]int{11 * 90000: longLine}), 4, 4},
		{"a long line across each boundary", longLineText(4*mib, map[int]int{
			11 * 80000: longLine, 11 * 180000: longLine, 11 * 280000: longLine,
		}), 4, 4},
		// Over the first two boundaries, not the third.
		{"a line longer than a chunk", longLineText(4*mib, map[int]int{11 * 80000: 11 * 200000}), 4, 3},
		{"one line without a newline", bytes.Repeat([]byte("x"), 4*mib), 4, 1},
		{"one line ending the file", append(bytes.Repeat([]byte("x"), 4*mib-1), '\n'), 4, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starts, err := chunkStarts(bytes.NewReader(tc.text), int64(len(tc.text)), tc.numChunks)
			if err != nil {
				t.Fatalf("chunkStarts: %v", err)
			}
			if len(starts) != tc.want {
				t.Errorf("%d chunk starts %v, want %d", len(starts), starts, tc.want)
			}
			if starts[0] != 0 {
				t.Errorf("first chunk starts at %d, want 0", starts[0])
			}
			for i := 1; i < len(starts); i++ {
				if starts[i] <= starts[i-1] || starts[i] >= int64(len(tc.text)) {
					t.Errorf("chunk start %d at %d does not follow %d inside the file", i, starts[i], starts[i-1])
				}
				if tc.text[starts[i]-1] != '\n' {
					t.Errorf("chunk start %d at %d is inside a line", i, starts[i])
				}
			}
		})
	}
}

// A file that is one long line is read once to plan its chunks, not once
// per tentative boundary: a search that ran past later boundaries
// without a newline answers for them too.
func TestChunkStarts_ReadAFileOfOneLineOnce(t *testing.T) {
	const size = 8 << 20
	text := bytes.Repeat([]byte("x"), size)
	r := counting.NewReaderAt(bytes.NewReader(text))
	starts, err := chunkStarts(r, size, 8)
	if err != nil {
		t.Fatalf("chunkStarts: %v", err)
	}
	if len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("chunk starts %v, want [0]", starts)
	}
	if got := r.Load(); got > size {
		t.Errorf("read %d bytes to plan the chunks of a %d-byte file, want at most its size", got, size)
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
