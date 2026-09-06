package trace

import (
	"context"
	"strings"
	"testing"
	"time"
)

// writeBigFrameSeekableFile builds a seekable-zstd file with frames as
// large as the ones rx writes by default (4 MB), and enough of them
// that a scan is still feeding ripgrep when something cancels it.
//
// Frame size is what makes this reproduce: the writer hands ripgrep one
// frame per write, so a canceled scan leaves a multi-megabyte write in
// flight rather than one that fits in the pipe buffer.
func writeBigFrameSeekableFile(t *testing.T) string {
	t.Helper()
	line := "line with NEEDLE in it, and padding that makes the frames add up\n"
	var b strings.Builder
	b.Grow(48 << 20)
	for b.Len() < 48<<20 {
		b.WriteString(line)
	}
	return writeSeekableZstdFile(t, []byte(b.String()), 4<<20)
}

// TestProcessSeekableReturnsWhenCancelled is the regression test for a
// search that never came back.
//
// The frame writer feeds an io.Pipe whose only reader is the copy into
// ripgrep's stdin. When ripgrep went away — killed because the context
// was canceled, or because a max_results cap fired — that copy stopped
// and the writer blocked on a pipe nobody was reading. The scan then
// waited on the writer forever: on the CLI it hung, and over HTTP it
// held the handler open and blocked shutdown.
//
// The assertion is termination, not speed: without the fix this never
// returns at all.
func TestProcessSeekableReturnsWhenCancelled(t *testing.T) {
	requireRipgrep(t)
	path := writeBigFrameSeekableFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := ProcessSeekable(
			ctx, path,
			map[string]string{"p1": "NEEDLE"}, []string{"p1"},
			nil, 0, 0, nil,
		)
		done <- err
	}()

	// Cancel while the frames are still being fed to ripgrep.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("ProcessSeekable did not return after its context was canceled; " +
			"the frame writer is stuck on a pipe nobody reads")
	}
}

// TestProcessSeekableWithACapReturns covers the same stall reached
// through a max_results cap, which is how a person meets it.
func TestProcessSeekableWithACapReturns(t *testing.T) {
	requireRipgrep(t)
	path := writeBigFrameSeekableFile(t)

	for _, limit := range []int{1, 5, 100} {
		t.Run("cap"+itoa(limit), func(t *testing.T) {
			cap := limit
			type result struct {
				matches []MatchRaw
				err     error
			}
			done := make(chan result, 1)
			go func() {
				m, _, _, err := ProcessSeekable(
					context.Background(), path,
					map[string]string{"p1": "NEEDLE"}, []string{"p1"},
					nil, 0, 0, &cap,
				)
				done <- result{matches: m, err: err}
			}()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("ProcessSeekable: %v", got.err)
				}
				if len(got.matches) != cap {
					t.Fatalf("got %d matches, want the cap of %d", len(got.matches), cap)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("ProcessSeekable did not return under a cap")
			}
		})
	}
}

// TestProcessSeekableCapKeepsTheFileReadable guards the other half of
// the same fix: a ripgrep killed by the cap must not be mistaken for a
// crash, which would turn the file into a skipped one and the search
// into an empty result.
func TestProcessSeekableCapKeepsTheFileReadable(t *testing.T) {
	requireRipgrep(t)
	path := writeBigFrameSeekableFile(t)

	limit := 3
	resp, err := New().RunWithOptions(
		context.Background(), []string{path}, []string{"NEEDLE"},
		Options{MaxResults: &limit, NoCache: true},
	)
	if err != nil {
		t.Fatalf("RunWithOptions: %v", err)
	}
	if len(resp.SkippedFiles) != 0 {
		t.Fatalf("the capped scan skipped %v", resp.SkippedFiles)
	}
	if len(resp.Matches) != limit {
		t.Fatalf("got %d matches, want %d", len(resp.Matches), limit)
	}
}
