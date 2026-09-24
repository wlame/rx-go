package samples

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/samplesanswer"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// headTestBytes is the head the correctness tests read: about 450
// lines of the generated logs, a fifth of their text.
const headTestBytes = 16 << 10

// headCopyFormats are the formats every head scenario is checked in,
// besides the plain file.
var headCopyFormats = []string{
	compressedcopy.Gzip, compressedcopy.Bzip2, compressedcopy.Xz,
	compressedcopy.Zstd, compressedcopy.SeekableZstd,
}

// headCopies writes text as a plain file and as a copy in each of
// headCopyFormats, each in its own directory, and returns their paths
// by name, the plain file under "plain". A format whose encoder the
// host lacks (bzip2) is left out.
func headCopies(t *testing.T, text []byte) map[string]string {
	t.Helper()
	copies := map[string]string{"plain": filepath.Join(t.TempDir(), "app.log")}
	writeFile(t, copies["plain"], text)
	for _, format := range headCopyFormats {
		encoded := compressedcopy.Encode(t, format, text)
		if encoded == nil {
			continue
		}
		path := filepath.Join(t.TempDir(), "app.log.compressed")
		writeFile(t, path, encoded)
		copies[format] = path
	}
	return copies
}

// headRequest is one request of a head scenario. Exactly one of lines,
// offsets and times is set.
type headRequest struct {
	lines, offsets string
	times          []string
	before, after  int
	maxLines       int
}

// String names the request in a failure.
func (r headRequest) String() string {
	switch {
	case r.lines != "":
		return "lines=" + r.lines
	case r.offsets != "":
		return "offsets=" + r.offsets
	}
	return "timestamps=" + strings.Join(r.times, "|")
}

// request turns r into the Request for path.
func (r headRequest) request(t testing.TB, path string) Request {
	t.Helper()
	req := Request{
		Path: path, Timestamps: r.times, BeforeContext: r.before, AfterContext: r.after,
		MaxLines: r.maxLines,
	}
	var err error
	if r.lines != "" {
		req.Lines, err = ParseCSV(r.lines)
	}
	if r.offsets != "" {
		req.Offsets, err = ParseCSV(r.offsets)
	}
	if err != nil {
		t.Fatalf("%s: %v", r, err)
	}
	return req
}

// headAnswerOf is answerOf with the file's path taken out of an error,
// so the copies of one text compare equal.
func headAnswerOf(path string, resp *rxtypes.SamplesResponse, err error) any {
	if err != nil {
		err = errors.New(strings.ReplaceAll(err.Error(), path, "PATH"))
	}
	return answerOf(resp, err)
}

// headTimedText is the log the head scenarios read: 2,000 timestamped
// lines one second apart, about 72 KB, with the continuation lines of
// orderedValues.
func headTimedText() []byte {
	return textOf(isoLines(orderedValues(2000)))
}

// earlyAndIndexed asks path for r from the head, requires the head to
// answer, then builds and stores the line index and requires the
// indexed lookup to answer the same (samplesanswer.ColdAndIndexed). It
// returns the early answer.
func earlyAndIndexed(t *testing.T, path string, r headRequest, head int64) any {
	t.Helper()
	return samplesanswer.ColdAndIndexed(t, path, timeIndexStep, func(t testing.TB) any {
		req := r.request(t, path)
		if stored, _ := index.LoadForSource(path); stored != nil {
			req.IndexLoader = StoredIndex
			resp, err := Resolve(context.Background(), req)
			return headAnswerOf(path, resp, err)
		}
		resp, answered, err := ResolveFromHead(context.Background(), req, head)
		if !answered {
			t.Fatalf("%s: the head of %d bytes did not answer (error %v)", r, head, err)
		}
		return headAnswerOf(path, resp, err)
	})
}

// requireAnsweredFromHead checks r on every copy of text: the head
// answers it, the answer equals the indexed answer, and every copy
// answers as the plain file does. It returns the plain file's answer.
func requireAnsweredFromHead(t *testing.T, text []byte, r headRequest, head int64) any {
	t.Helper()
	copies := headCopies(t, text)
	want := earlyAndIndexed(t, copies["plain"], r, head)
	for name, path := range copies {
		if name == "plain" {
			continue
		}
		got := earlyAndIndexed(t, path, r, head)
		samplesanswer.RequireAgree(t, fmt.Sprintf("%s, %s copy against the plain file", r, name), got, want)
	}
	return want
}

// TestResolveFromHead_AnswersAsTheIndexedLookupDoes covers every mode
// on a request whose lines lie in the head: the early answer is the
// one the file's line index gives, line_timestamps and time_format
// included, for the plain file and each compressed copy.
func TestResolveFromHead_AnswersAsTheIndexedLookupDoes(t *testing.T) {
	text := headTimedText()
	requests := []headRequest{
		{lines: "5", before: 3, after: 3},
		{lines: "1-40"},
		{lines: "0,100,200", before: 2, after: 1},
		// Line 14 has no timestamp of its own: with no context before
		// it, its effective timestamp is read back from the text.
		{lines: "14", before: 0, after: 2},
		{offsets: "0", before: 3, after: 3},
		{offsets: "1000,5000", before: 2, after: 2},
		{offsets: "500-2000"},
		{times: []string{iso(timeBase + 50_000)}, before: 2, after: 2},
		{times: []string{iso(timeBase+10_000) + ".." + iso(timeBase+60_000)}},
		{times: []string{iso(timeBase - 3_600_000)}, before: 0, after: 1},
	}
	for _, r := range requests {
		t.Run(r.String(), func(t *testing.T) {
			got := requireAnsweredFromHead(t, text, r, headTestBytes)
			if doc, ok := got.(map[string]any); ok && doc["error"] != nil {
				t.Fatalf("%s: answered with an error: %v", r, doc["error"])
			}
		})
	}
}

// TestResolveFromHead_DoesNotAnswerPastTheHead covers the requests the
// head cannot answer: a position counted back from the end, a line, a
// range or an offset past the head, and a time whose line, or whose
// range's end, lies past it or needs the file's last timestamp. None
// is answered, none is an error, and the caller goes on as before.
func TestResolveFromHead_DoesNotAnswerPastTheHead(t *testing.T) {
	text := headTimedText()
	requests := []headRequest{
		{lines: "-1"},
		{lines: "5,-5"},
		{lines: "1-1000"},
		{lines: "900"},
		{lines: "440", before: 0, after: 200},
		{offsets: "-1"},
		{offsets: fmt.Sprint(headTestBytes)},
		{offsets: fmt.Sprintf("100-%d", headTestBytes)},
		{times: []string{iso(timeBase + 1_500_000)}},
		{times: []string{iso(timeBase + 10_000_000)}},
		{times: []string{iso(timeBase+10_000) + ".."}},
		{times: []string{iso(timeBase+10_000) + ".." + iso(timeBase+1_500_000)}},
		// A time of day needs the file's last timestamp, at its end.
		{times: []string{"07:31:00"}},
	}
	for name, path := range headCopies(t, text) {
		for _, r := range requests {
			t.Run(name+"/"+r.String(), func(t *testing.T) {
				resp, answered, err := ResolveFromHead(context.Background(), r.request(t, path), headTestBytes)
				if answered || resp != nil || err != nil {
					t.Fatalf("answered %v, response %v, error %v; want not answered", answered, resp != nil, err)
				}
			})
		}
	}
}

// TestResolveFromHead_ATextAsLongAsTheHeadIsAnswered covers the
// boundary: a head exactly as long as the text holds every line, and
// one byte shorter does not hold the last.
func TestResolveFromHead_ATextAsLongAsTheHeadIsAnswered(t *testing.T) {
	text := textOf(isoLines(orderedValues(30)))
	size := int64(len(text))
	requireAnsweredFromHead(t, text, headRequest{lines: "1-30"}, size)
	requireAnsweredFromHead(t, text, headRequest{lines: "25", before: 2, after: 100}, size)
	requireAnsweredFromHead(t, text, headRequest{lines: "1-29"}, size-1)

	for name, path := range headCopies(t, text) {
		r := headRequest{lines: "1-30"}
		if _, answered, err := ResolveFromHead(context.Background(), r.request(t, path), size-1); answered || err != nil {
			t.Errorf("%s: a head one byte short of the text answered %v, error %v; want not answered", name, answered, err)
		}
	}
}

// TestResolveFromHead_AnswersARefusalAsTheLookupWould covers the errors
// that are the answer itself: a request the head holds whose answer
// passes a limit, or names a time the file cannot answer, is answered
// with the error Resolve gives, not passed on to an index build.
func TestResolveFromHead_AnswersARefusalAsTheLookupWould(t *testing.T) {
	timed := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, timed, headTimedText())
	untimed := filepath.Join(t.TempDir(), "plain.log")
	writeFile(t, untimed, []byte(strings.Repeat("no time on this line\n", 200)))

	cases := []struct {
		name string
		path string
		r    headRequest
		want func(error) bool
	}{
		{"too many lines", timed, headRequest{lines: "1-100", maxLines: 10},
			func(err error) bool { return errors.Is(err, ErrTooManyLines) }},
		{"no timestamps", untimed, headRequest{times: []string{iso(timeBase)}}, IsUsageError},
		{"not a time", timed, headRequest{times: []string{"not a time"}}, IsUsageError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, answered, err := ResolveFromHead(context.Background(), c.r.request(t, c.path), headTestBytes)
			if !answered || !c.want(err) {
				t.Fatalf("answered %v, error %v; want answered with the refusal", answered, err)
			}
		})
	}
}

// headBudgetText is a log of about size bytes whose lines carry a
// random tail, so a compressed copy is about as long as the text and a
// byte count in compressed bytes says how much text was read.
func headBudgetText(size int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	for n := 1; b.Len() < size; n++ {
		stamp := time.UnixMilli(timeBase + int64(n)*1000).UTC().Format("2006-01-02 15:04:05.000")
		fmt.Fprintf(&b, "%s INFO LINE %d %016x%016x%016x%016x\n", stamp, n,
			rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	}
	return []byte(b.String())
}

// TestResolveFromHead_ReadsAtMostTheHead is the bounded-read check: a
// request the head cannot answer stops at the end of the head, in every
// mode and format, rather than reading the file it would have to read
// in full. The count is of the file's bytes, compressed for a
// compressed copy: the format detection reads the first mebibyte of
// text, and the pass reads the head and one byte past it, so the two
// read twice the head's share of the file, plus what a decompressor
// reads ahead.
func TestResolveFromHead_ReadsAtMostTheHead(t *testing.T) {
	const (
		head     = int64(1 << 20)
		textSize = 12 << 20
		// readAhead is what the two decompressors, the detection's and the
		// pass's, may read past the compressed bytes of the text they have
		// given out.
		readAhead = 2 * (256 << 10)
	)
	text := headBudgetText(textSize)
	lineCount := strings.Count(string(text), "\n")
	paths := map[string]string{"plain": filepath.Join(t.TempDir(), "app.log")}
	writeFile(t, paths["plain"], text)
	for _, format := range []string{compressedcopy.Gzip, compressedcopy.Zstd} {
		paths[format] = filepath.Join(t.TempDir(), "app.log.compressed")
		writeFile(t, paths[format], compressedcopy.Encode(t, format, text))
	}
	paths[compressedcopy.SeekableZstd] = filepath.Join(t.TempDir(), "app.log.zst")
	seekablefile.Write(t, paths[compressedcopy.SeekableZstd], seekablefile.SplitEvery(text, 256<<10))

	// The offset range ends on the head's last byte, inside a line that
	// runs past the head, so the range's last line is not in the head.
	if text[head-1] == '\n' {
		t.Fatalf("the generated text has a line break at byte %d; the offsets case needs a line across the head", head-1)
	}
	requests := []headRequest{
		{lines: fmt.Sprintf("1-%d", lineCount)},
		{lines: fmt.Sprint(lineCount / 2)},
		{offsets: fmt.Sprintf("10-%d", head-1)},
		{times: []string{iso(timeBase + int64(lineCount)*1000)}},
	}
	for name, path := range paths {
		for _, r := range requests {
			t.Run(name+"/"+r.String(), func(t *testing.T) {
				counter := withCountingOpen(t)
				_, answered, err := ResolveFromHead(context.Background(), r.request(t, path), head)
				if answered || err != nil {
					t.Fatalf("answered %v, error %v; want not answered", answered, err)
				}
				budget := 2*head*fileSize(t, path)/int64(len(text)) + readAhead
				if got := counter.Load(); got > budget {
					t.Fatalf("read %d bytes, budget %d (the file is %d bytes)", got, budget, fileSize(t, path))
				}
			})
		}
	}
}

// TestResolveFromHead_ReadsNothingItCannotUse covers the requests ruled
// out before any read: a head of 0 (the early answer switched off), a
// position counted back from the end, and an offset past the head.
func TestResolveFromHead_ReadsNothingItCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, path, headTimedText())
	cases := []struct {
		r    headRequest
		head int64
	}{
		{headRequest{lines: "1-10"}, 0},
		{headRequest{lines: "1-10"}, -1},
		{headRequest{lines: "-1"}, headTestBytes},
		{headRequest{offsets: "-10"}, headTestBytes},
		{headRequest{offsets: fmt.Sprint(headTestBytes)}, headTestBytes},
		{headRequest{offsets: fmt.Sprintf("0-%d", headTestBytes)}, headTestBytes},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/head=%d", c.r, c.head), func(t *testing.T) {
			counter := withCountingOpen(t)
			_, answered, err := ResolveFromHead(context.Background(), c.r.request(t, path), c.head)
			if answered || err != nil {
				t.Fatalf("answered %v, error %v; want not answered", answered, err)
			}
			if got := counter.Load(); got != 0 {
				t.Fatalf("read %d bytes, want none", got)
			}
		})
	}
}

// fileSize returns the size of the file at path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
