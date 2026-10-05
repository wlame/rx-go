package samples

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
)

// reachLineBytes is the length of every line of reachText, line break
// included, so the reach of any head is known without reading it.
const reachLineBytes = 100

// reachTestHead is the head the reach tests read. It holds 163 whole
// lines of reachText (16,300 bytes) and the first 84 bytes of line 164.
const reachTestHead = 16 << 10

// reachOfTestHead is the reach of reachTestHead over reachText.
var reachOfTestHead = HeadReach{Head: reachTestHead, Lines: 163, End: 163 * reachLineBytes}

// reachText is 1,000 lines of reachLineBytes bytes, each reading
// "LINE <n>" and padded with dots.
func reachText() []byte {
	var b strings.Builder
	for n := 1; n <= 1000; n++ {
		line := fmt.Sprintf("LINE %d ", n)
		b.WriteString(line + strings.Repeat(".", reachLineBytes-1-len(line)) + "\n")
	}
	return []byte(b.String())
}

// An attempt that runs past the head reports how far the head reaches:
// the lines whose line break lies inside it and the offset just past
// the last of them, for the plain file and every compressed copy.
func TestResolveFromHeadWithReach_MeasuresTheReachOfTheHead(t *testing.T) {
	requests := []headRequest{
		{lines: "500"},
		{lines: "1-1000"},
		{lines: "160", before: 0, after: 4},
	}
	for name, path := range headCopies(t, reachText()) {
		for _, r := range requests {
			t.Run(name+"/"+r.String(), func(t *testing.T) {
				_, answered, reach, err := ResolveFromHeadWithReach(context.Background(), r.request(t, path), reachTestHead, nil)
				if answered || err != nil {
					t.Fatalf("answered %v, error %v; want not answered", answered, err)
				}
				if reach == nil || *reach != reachOfTestHead {
					t.Fatalf("reach %+v, want %+v", reach, reachOfTestHead)
				}
			})
		}
	}
}

// With the reach known, a request that needs a line past it, or an
// offset at or past the end of its last line, is not answered and reads
// nothing: the head is known not to hold it.
func TestResolveFromHeadWithReach_ReadsNothingPastAKnownReach(t *testing.T) {
	requests := []headRequest{
		{lines: "164"},
		{lines: "1-164"},
		{lines: "160", before: 0, after: 4},
		{lines: "5,164", before: 0, after: 0},
		{offsets: fmt.Sprint(reachOfTestHead.End)},
		{offsets: fmt.Sprintf("10-%d", reachOfTestHead.End)},
	}
	for name, path := range headCopies(t, reachText()) {
		for _, r := range requests {
			t.Run(name+"/"+r.String(), func(t *testing.T) {
				counter := withCountingOpen(t)
				known := reachOfTestHead
				_, answered, reach, err := ResolveFromHeadWithReach(context.Background(), r.request(t, path), reachTestHead, &known)
				if answered || err != nil {
					t.Fatalf("answered %v, error %v; want not answered", answered, err)
				}
				if reach == nil || *reach != reachOfTestHead {
					t.Fatalf("reach %+v, want the known %+v", reach, reachOfTestHead)
				}
				if got := counter.Load(); got != 0 {
					t.Fatalf("read %d bytes, want none", got)
				}
			})
		}
	}
}

// With the reach known, a request inside it is still answered from the
// head, up to and including the last line of the reach, with the
// answer the lookup without an index gives.
func TestResolveFromHeadWithReach_AnswersInsideAKnownReach(t *testing.T) {
	requests := []headRequest{
		{lines: "1-163"},
		{lines: "160", before: 2, after: 3},
		{lines: "5,100", before: 1, after: 1},
		{offsets: "100", before: 0, after: 0},
	}
	for name, path := range headCopies(t, reachText()) {
		for _, r := range requests {
			t.Run(name+"/"+r.String(), func(t *testing.T) {
				known := reachOfTestHead
				resp, answered, _, err := ResolveFromHeadWithReach(context.Background(), r.request(t, path), reachTestHead, &known)
				if !answered || err != nil {
					t.Fatalf("answered %v, error %v; want answered", answered, err)
				}
				coldResp, coldErr := Resolve(context.Background(), r.request(t, path))
				samplesanswer.RequireAgree(t, r.String(), headAnswerOf(path, resp, err), headAnswerOf(path, coldResp, coldErr))
			})
		}
	}
}

// A reach measured under another head size says nothing about this
// head: the request is tried, and the reach of this head is measured.
func TestResolveFromHeadWithReach_IgnoresTheReachOfAnotherHead(t *testing.T) {
	for name, path := range headCopies(t, reachText()) {
		t.Run(name, func(t *testing.T) {
			counter := withCountingOpen(t)
			other := HeadReach{Head: 2 * reachTestHead, Lines: 327, End: 327 * reachLineBytes}
			r := headRequest{lines: "200"}
			_, answered, reach, err := ResolveFromHeadWithReach(context.Background(), r.request(t, path), reachTestHead, &other)
			if answered || err != nil {
				t.Fatalf("answered %v, error %v; want not answered", answered, err)
			}
			if counter.Load() == 0 {
				t.Fatal("read nothing; want the head tried")
			}
			if reach == nil || *reach != reachOfTestHead {
				t.Fatalf("reach %+v, want %+v", reach, reachOfTestHead)
			}
		})
	}
}
