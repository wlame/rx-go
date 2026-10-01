package trace

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/counting"
)

// chunkText is ten short lines and one longer than the read buffer, so
// a backward read has to cross a buffer boundary inside a line.
func chunkText() (text []byte, starts []int64) {
	var b bytes.Buffer
	for n := 1; n <= 12; n++ {
		starts = append(starts, int64(b.Len()))
		if n == 6 {
			fmt.Fprintf(&b, "line %d %s\n", n, strings.Repeat("y", chunkCopyBufferBytes+100))
			continue
		}
		fmt.Fprintf(&b, "line %d\n", n)
	}
	return b.Bytes(), starts
}

func TestReadLinesBefore_ReturnsTheLinesThatEndAtTheOffset(t *testing.T) {
	text, starts := chunkText()
	cases := []struct {
		name      string
		lineAfter int // 1-based line that starts at the offset
		lines     int
		wantFrom  int // 1-based first line returned; 0 for nothing
	}{
		{"no context", 9, 0, 0},
		{"two lines", 9, 2, 7},
		{"through the long line", 9, 4, 5},
		{"more lines than the text has", 3, 5, 1},
		{"at the start of the text", 1, 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := counting.NewReaderAt(bytes.NewReader(text))
			offset := starts[tc.lineAfter-1]

			got, err := readLinesBefore(r, offset, tc.lines)

			if err != nil {
				t.Fatalf("readLinesBefore: %v", err)
			}
			var want []byte
			if tc.wantFrom > 0 {
				want = text[starts[tc.wantFrom-1]:offset]
			}
			if !bytes.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
			// The backward search reads whole buffers, and the lines are
			// read once more once found.
			if budget := int64(2*len(want) + 2*chunkCopyBufferBytes); r.Load() > budget {
				t.Errorf("read %d bytes, budget %d", r.Load(), budget)
			}
		})
	}
}

func TestFeedChunk_HandsRipgrepTheLinesAroundTheChunkAndCountsOnlyTheChunk(t *testing.T) {
	text, starts := chunkText()
	task := FileTask{Offset: starts[7], Count: starts[9] - starts[7]} // lines 8 and 9
	leadIn := text[starts[5]:starts[7]]                               // lines 6 and 7
	cases := []struct {
		name     string
		after    int
		wantTail []byte
	}{
		{"no trailing context", 0, nil},
		{"two lines after", 2, text[starts[9]:starts[11]]},
		{"more lines than the text has", 9, text[starts[9]:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := counting.NewReaderAt(bytes.NewReader(text))
			var rg bytes.Buffer
			input := chunkInput{task: task, leadIn: leadIn, leadStart: starts[5], leadLines: 2}

			fed, err := feedChunk(r, input, tc.after, &rg)

			if err != nil {
				t.Fatalf("feedChunk: %v", err)
			}
			want := append(append(append([]byte(nil), leadIn...), text[task.Offset:task.EndOffset()]...), tc.wantTail...)
			if !bytes.Equal(rg.Bytes(), want) {
				t.Errorf("ripgrep got %q, want %q", rg.Bytes(), want)
			}
			if fed.copied != task.Count || fed.newlines != 2 {
				t.Errorf("counted %d bytes and %d line breaks, want the chunk's %d and 2", fed.copied, fed.newlines, task.Count)
			}
			// The chunk and the tail, plus at most one buffer read past
			// the tail's last line break.
			if budget := task.Count + int64(len(tc.wantTail)) + chunkCopyBufferBytes; r.Load() > budget {
				t.Errorf("read %d bytes, budget %d", r.Load(), budget)
			}
		})
	}
}

// chunkInput translates ripgrep's positions and line numbers, which
// count the lead-in, into the file's and the chunk's.
func TestChunkInput_TranslatesRipgrepsPositions(t *testing.T) {
	input := chunkInput{task: FileTask{Offset: 1000, Count: 500}, leadStart: 900, leadLines: 3}
	if got := input.fileOffset(0); got != 900 {
		t.Errorf("fileOffset(0) = %d, want 900", got)
	}
	if got := input.chunkLine(4); got != 1 {
		t.Errorf("chunkLine(4) = %d, want 1, the chunk's first line", got)
	}
	if got := input.chunkLine(1); got != -2 {
		t.Errorf("chunkLine(1) = %d, want -2, three lines before the chunk's first", got)
	}
	for offset, want := range map[int64]bool{999: false, 1000: true, 1499: true, 1500: false} {
		if got := input.owns(offset); got != want {
			t.Errorf("owns(%d) = %v, want %v", offset, got, want)
		}
	}
}
