package samples

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// largeTimedLog writes a plain log of n ordered timestamped lines of
// about 150 bytes and returns its path, its lines and its size.
func largeTimedLog(t *testing.T, n int) (string, []timedLine, int64) {
	t.Helper()
	values := make([]int64, n)
	for i := range values {
		values[i] = timeBase + int64(i)*100
	}
	lines := isoLines(values)
	for i := range lines {
		lines[i].text += " " + strings.Repeat("x", 110)
	}
	path := filepath.Join(t.TempDir(), "big.log")
	text := textOf(lines)
	writeFile(t, path, text)
	return path, lines, int64(len(text))
}

// With an index, a time query reads at most one index step to find its
// line, and then what --lines reads for that line: the search starts at
// the checkpoint whose max_before says the answer is past it.
func TestBudget_IndexedTimeSearchReadsOneStep(t *testing.T) {
	const step = 64 * 1024
	path, lines, size := largeTimedLog(t, 20_000)
	idx, err := index.Build(path, index.BuildOptions{StepBytes: step})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	loader := func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
	target := lines[17_345].ms + 50 // between two lines: the answer is line 17,347

	counter := withCountingOpen(t)
	byTime, err := Resolve(t.Context(), Request{Path: path, Timestamps: []string{iso(target)}, BeforeContext: 3, AfterContext: 3, IndexLoader: loader})
	if err != nil {
		t.Fatalf("by time: %v", err)
	}
	timeRead := counter.Load()
	if got := byTime.Timestamps[iso(target)]; got != 17_347 {
		t.Fatalf("line %d, want 17347", got)
	}

	counter.Store(0)
	if _, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: 17_347}}, BeforeContext: 3, AfterContext: 3, IndexLoader: loader}); err != nil {
		t.Fatalf("by line: %v", err)
	}
	lineRead := counter.Load()

	// One step, the search buffer past the answer, and the longest line.
	budget := lineRead + step + int64(searchBufferBytes(true)) + 256
	if timeRead > budget {
		t.Errorf("by time read %d bytes of %d; --lines read %d; budget %d", timeRead, size, lineRead, budget)
	}
}

// Without an index, the timestamp format of every answer is detected
// from at most a mebibyte of the text.
func TestBudget_TimeFormatDetectionReadsOnlyTheHead(t *testing.T) {
	path, _, size := largeTimedLog(t, 20_000)
	counter := withCountingOpen(t)
	resp, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: 1}}, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp.TimeFormat == nil {
		t.Fatalf("time_format null for a timestamped log")
	}
	// The head, and line 1 with its read buffer.
	budget := int64(timestamps.SampleBytes) + 4*1024
	if read := counter.Load(); read > budget {
		t.Errorf("read %d bytes of %d; budget %d", read, size, budget)
	}
}

// The read back from the end for a file's last timestamp reads one
// step and the window of one line past it, when that step holds a
// timestamped line.
func TestBudget_LastTimestampReadsOneStepFromTheEnd(t *testing.T) {
	path, lines, size := largeTimedLog(t, 20_000)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	text := counting.NewReaderAt(f)
	stamp, found, err := lastStampFromEnd(context.Background(), text, size, parserFor(t, textOf(lines)), size)
	if err != nil || !found || stamp.Ms != lines[len(lines)-1].ms {
		t.Fatalf("last stamp %+v, %v, %v; want %d", stamp, found, err, lines[len(lines)-1].ms)
	}
	if read, budget := text.Load(), int64(tailStepBytes+timestamps.WindowBytes+1); read > budget {
		t.Errorf("read %d bytes of %d; budget %d", read, size, budget)
	}
}

// parserFor detects the format of text and returns its parser.
func parserFor(t testing.TB, text []byte) *timestamps.Parser {
	t.Helper()
	format, ok := timestamps.Detect(text)
	if !ok {
		t.Fatalf("no format detected")
	}
	parser, err := timestamps.NewParser(format, 0)
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	return parser
}

// awkwardLines are lines where the bytes the parser sees depend on
// where the line ends: long lines, carriage-return runs at and past the
// window's edge, a timestamp that ends at the edge, an unterminated
// last line.
func awkwardLines(rng *rand.Rand) [][]byte {
	stamp := "2025-12-10 07:30:00.000"
	var lines [][]byte
	for i := range 300 {
		var line string
		switch i % 7 {
		case 0:
			line = stamp + " INFO " + strings.Repeat("y", rng.IntN(9000))
		case 1:
			line = stamp + strings.Repeat("\r", rng.IntN(6000))
		case 2:
			line = strings.Repeat(" ", 97) + stamp + strings.Repeat("\r", rng.IntN(200))
		case 3:
			line = "    at frame " + strings.Repeat("z", rng.IntN(300))
		case 4:
			line = stamp + strings.Repeat("\r", 105) + "x" + strings.Repeat("\r", rng.IntN(5000))
		case 5:
			// The window's last byte is the \r of a \r\n line break.
			line = strings.Repeat("x", timestamps.WindowBytes-1) + "\r"
		default:
			line = fmt.Sprintf("2025-12-10 07:30:%02d.%03d end", rng.IntN(60), rng.IntN(1000))
		}
		lines = append(lines, []byte(line+"\n"))
	}
	lines = append(lines, []byte(stamp+" unterminated\r\r"))
	return lines
}

// The search pass and the read back from the end see each line as an
// index build does (index.LineStamp over the whole line), however long
// the line and wherever its \r bytes are.
func TestStampReaders_AgreeWithTheIndexWalk(t *testing.T) {
	lines := awkwardLines(rand.New(rand.NewPCG(3, 5)))
	text := bytes.Join(lines, nil)
	parser, err := timestamps.NewParser(timestamps.Format{Family: timestamps.FamilyISO}, 0)
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	reader := newStampReader(bytes.NewReader(text), parser, 4096)
	var lastWant timestamps.Stamp
	for i, line := range lines {
		want, wantOK := index.LineStamp(parser, line)
		if wantOK {
			lastWant = want
		}
		got, gotOK, length, err := reader.next()
		if err != nil || length != int64(len(line)) || got != want || gotOK != wantOK {
			t.Fatalf("line %d (%d bytes): got %+v %v length %d (%v), want %+v %v",
				i+1, len(line), got, gotOK, length, err, want, wantOK)
		}
		// The read back from the end, from this line's start, with one
		// byte past the window in hand, and with a few kilobytes.
		start := int64(len(bytes.Join(lines[:i], nil)))
		content := bytes.TrimRight(line, "\r\n")
		wantWindow := content[:min(len(content), timestamps.WindowBytes)]
		for _, inHand := range []int64{timestamps.WindowBytes + 1, 4096} {
			rest := text[start:min(int64(len(text)), start+inHand)]
			endsLine := onlyCarriageReturnsBeforeLineEnd(text[start+int64(len(rest)):], true)
			window := lineWindow(rest, endsLine)
			if !bytes.Equal(window, wantWindow) {
				t.Fatalf("line %d, %d bytes in hand: window %q, want %q", i+1, inHand, window, wantWindow)
			}
		}
	}
	if _, _, length, _ := reader.next(); length != 0 {
		t.Fatalf("a line after the last: %d bytes", length)
	}
	got, found, err := lastStampFromEnd(context.Background(), bytes.NewReader(text), int64(len(text)), parser, int64(len(text)))
	if err != nil || !found || got != lastWant {
		t.Fatalf("last stamp %+v %v %v, want %+v", got, found, err, lastWant)
	}
}

// When the last mebibyte of a file holds no timestamp (a long
// traceback), the read back goes one step further.
func TestLastStampFromEnd_StepsBackPastATimelessTail(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("2025-12-10 07:30:00.000 first\n2025-12-10 07:30:01.000 last stamped\n")
	for b.Len() < 3*tailStepBytes {
		b.WriteString("    at a frame of a very long traceback\n")
	}
	text := b.Bytes()
	got, found, err := lastStampFromEnd(context.Background(), bytes.NewReader(text), int64(len(text)), parserFor(t, text[:70]), int64(len(text)))
	if err != nil || !found || got.Ms != timeBase+1000 {
		t.Fatalf("last stamp %+v %v %v, want 07:30:01", got, found, err)
	}
}

// tailLines are texts whose lines end in long runs of \r bytes: runs
// about a step long and longer, ended by a \n, by another byte, or by
// the end of the text, after lines with a timestamp.
func tailLines() map[string][]byte {
	const stamp = "2025-12-10 07:30:05.000"
	texts := map[string][]byte{}
	for _, run := range []int{tailStepBytes - 60, tailStepBytes + 60, 3 * tailStepBytes} {
		for _, ending := range []string{"\n", "x\n", "", "\nabc"} {
			var b bytes.Buffer
			b.WriteString("2025-12-10 07:30:00.000 first\n")
			b.WriteString(stamp + strings.Repeat("\r", run) + ending)
			b.WriteString("abc" + strings.Repeat("\r", run) + "\r" + strings.Repeat("\r", timestamps.WindowBytes) + ending)
			texts[fmt.Sprintf("run %d ending %q", run, ending)] = b.Bytes()
		}
	}
	texts["awkward lines"] = bytes.Join(awkwardLines(rand.New(rand.NewPCG(7, 11))), nil)
	return texts
}

// tailWindow is one line start the read back visits and the window it
// hands the parser.
type tailWindow struct {
	start  int64
	window string
}

// The read back from the end visits every line that starts in the last
// limit bytes, last first, and hands the parser each line's content cut
// to timestamps.WindowBytes, as an index build does (index.LineStamp
// over the whole line), however far a run of \r bytes reaches.
func TestTailWindows_AreTheLinesAsTheIndexSeesThem(t *testing.T) {
	for name, text := range tailLines() {
		size := int64(len(text))
		for _, limit := range []int64{size, 2*tailStepBytes + 512} {
			t.Run(fmt.Sprintf("%s limit %d", name, limit), func(t *testing.T) {
				floor := max(0, size-limit)
				var want []tailWindow
				start := int64(0)
				for _, line := range bytes.SplitAfter(text, []byte("\n")) {
					if len(line) > 0 && start >= floor {
						content := bytes.TrimRight(line, "\r\n")
						want = append(want, tailWindow{start, string(content[:min(len(content), timestamps.WindowBytes)])})
					}
					start += int64(len(line))
				}
				slices.Reverse(want)

				var got []tailWindow
				err := tailWindows(context.Background(), bytes.NewReader(text), size, limit, func(start int64, window []byte) bool {
					got = append(got, tailWindow{start, string(window)})
					return false
				})
				if err != nil {
					t.Fatalf("tailWindows: %v", err)
				}
				if len(got) != len(want) {
					t.Fatalf("%d line starts visited, want %d", len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("line start %d: got %d %q, want %d %q", i, got[i].start, got[i].window, want[i].start, want[i].window)
					}
				}
			})
		}
	}
}

// tracebackHeavyLog is a log whose first 5,000 lines hold a record
// every 50 lines (enough for detection to find the format), and whose
// later lines are one long traceback, every line about 100 bytes, so a
// window inside it reads back for its record.
func tracebackHeavyLog(lines int) []byte {
	var b strings.Builder
	for n := 1; n <= lines; n++ {
		if n <= 5000 && n%50 == 1 {
			fmt.Fprintf(&b, "%s ERROR LINE %d failed\n", time.UnixMilli(timeBase+int64(n)).UTC().Format("2006-01-02 15:04:05.000"), n)
			continue
		}
		fmt.Fprintf(&b, "    at com.example.Frame%05d.call(Frame.java:%d) LINE %d %s\n", n, n, n, strings.Repeat("x", 40))
	}
	return []byte(b.String())
}

// The read back for a sample whose first line has no timestamp reads at
// most RX_TIMESTAMP_LOOKBACK_KB KiB and one byte more, from a plain
// file and from a seekable one by position, cold and with an index; a
// stream-compressed copy cannot be entered there and decompresses its
// text up to the line once more. The extra bytes are measured as the
// difference from the same request with a lookback of 0, which reads
// nothing back. Line 5300 has its record 35 KB back; line 7000 has none
// within 64 KiB, so its read covers the whole lookback.
func TestBudget_LookbackReadsAtMostTheSetting(t *testing.T) {
	const frameText = 4096
	text := tracebackHeavyLog(8000)
	dir := t.TempDir()
	paths := map[string]string{
		"plain":    filepath.Join(dir, "app.log"),
		"gzip":     filepath.Join(dir, "app.log.gz"),
		"seekable": filepath.Join(dir, "app.log.zst"),
	}
	writeFile(t, paths["plain"], text)
	writeFile(t, paths["gzip"], compressedcopy.Encode(t, compressedcopy.Gzip, text))
	seekablefile.Write(t, paths["seekable"], seekablefile.SplitEvery(text, frameText))
	lookback := int64(64 * 1024)
	for name, path := range paths {
		for loaderName, loader := range loadersFor(t, path) {
			read := func(lookbackKB string, line int64) (int64, *rxtypes.SamplesResponse) {
				t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", lookbackKB)
				counter := withCountingOpen(t)
				resp, err := Resolve(t.Context(), Request{Path: path, Lines: []OffsetOrRange{{Start: line}}, IndexLoader: loader})
				if err != nil {
					t.Fatalf("%s %s: resolve: %v", name, loaderName, err)
				}
				return counter.Load(), resp
			}
			for _, line := range []int64{5300, 7000} {
				without, _ := read("0", line)
				with, resp := read("64", line)
				extra := with - without
				budget := lookback + 1
				switch name {
				case "gzip":
					// The text up to the line again, and what the
					// decoder reads ahead.
					budget = without + decoderReadAhead
				case "seekable":
					// The frames that hold the bytes read back: the
					// lookback, and a frame at each end that it
					// covers in part.
					budget = lookback + 1 + 2*frameText
				}
				if extra <= 0 || extra > budget {
					t.Errorf("%s %s line %d: the read back cost %d bytes (%d against %d); budget %d",
						name, loaderName, line, extra, with, without, budget)
				}
				got := resp.LineTimestamps[strconv.FormatInt(line, 10)]
				if wantNull := line == 7000; len(got) != 1 || (got[0] == nil) != wantNull {
					t.Errorf("%s %s line %d: line_timestamps %v", name, loaderName, line, got)
				}
			}
		}
	}
}

// crlfTail is a log whose first mebibyte holds timestamped lines and
// whose tail is lines lines of timestamps.WindowBytes-1 bytes ended by
// \r\n: the window the parser looks at ends with that \r, and the \n is
// one byte past it.
func crlfTail(lines int) []byte {
	var b bytes.Buffer
	for b.Len() < tailStepBytes {
		b.WriteString("2025-12-10 07:30:00.000 INFO first\n")
	}
	for range lines {
		b.WriteString(strings.Repeat("x", timestamps.WindowBytes-1) + "\r\n")
	}
	return b.Bytes()
}

// The read back from the end for the last timestamp reads each byte of
// a tail of \r\n lines about once, on a plain file and on a seekable
// one: a step reads its mebibyte, one byte before it and the window
// past it, and nothing further to find where a line ends.
func TestBudget_LastTimestampReadsACRLFTailOnce(t *testing.T) {
	text := crlfTail(20_000)
	parser := parserFor(t, text[:200])
	steps := int64(len(text)/tailStepBytes + 1)
	// Past its mebibyte, a step reads the window after it and one byte
	// before it.
	perStep := int64(timestamps.WindowBytes + 1)

	plain := counting.NewReaderAt(bytes.NewReader(text))
	stamp, found, err := lastStampFromEnd(context.Background(), plain, int64(len(text)), parser, int64(len(text)))
	if err != nil || !found || stamp.Ms != timeBase {
		t.Fatalf("plain: last stamp %+v %v %v", stamp, found, err)
	}
	if read, budget := plain.Load(), int64(len(text))+steps*perStep; read > budget {
		t.Errorf("plain: read %d bytes of a %d-byte text; budget %d", read, len(text), budget)
	}

	path := filepath.Join(t.TempDir(), "tail.log.zst")
	seekablefile.Write(t, path, seekablefile.SplitEvery(text, 256*1024))
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	table, err := seekable.ReadSeekTable(f, info.Size())
	if err != nil {
		t.Fatalf("seek table: %v", err)
	}
	file := counting.NewReaderAt(f)
	compressed := &seekableTextAt{ctx: context.Background(), file: file, table: table, decoder: seekable.NewDecoder()}
	stamp, found, err = lastStampFromEnd(context.Background(), compressed, int64(len(text)), parser, int64(len(text)))
	if err != nil || !found || stamp.Ms != timeBase {
		t.Fatalf("seekable: last stamp %+v %v %v", stamp, found, err)
	}
	// Each frame is decoded by the steps that cover it, a few times
	// at most, never once per line.
	if read, budget := file.Load(), 8*info.Size(); read > budget {
		t.Errorf("seekable: read %d compressed bytes of a %d-byte file; budget %d", read, info.Size(), budget)
	}
}

// countDecodedFrames counts the bytes the reads by position of a
// seekable file decode, for the duration of the test.
func countDecodedFrames(t *testing.T) *atomic.Int64 {
	t.Helper()
	decoded := new(atomic.Int64)
	orig := decodeFrameAt
	decodeFrameAt = func(d *seekable.Decoder, file io.ReaderAt, index int, table *seekable.SeekTable) ([]byte, error) {
		data, err := orig(d, file, index, table)
		decoded.Add(int64(len(data)))
		return data, err
	}
	t.Cleanup(func() { decodeFrameAt = orig })
	return decoded
}

// Many samples whose first lines have no timestamp, in a seekable file
// of large frames, decode each frame their read back covers once: the
// read back sweeps the keys in ascending order and keeps the frame it
// decoded last. Two hundred keys inside the first two frames used to
// decode those frames once per key.
func TestBudget_LookbackDecodesEachSeekableFrameOnce(t *testing.T) {
	const frameText = 1 << 20
	text := tracebackHeavyLog(40_000)
	path := filepath.Join(t.TempDir(), "app.log.zst")
	seekablefile.Write(t, path, seekablefile.SplitEvery(text, frameText))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	var lines []OffsetOrRange
	for line := int64(5100); line < 7100; line += 10 {
		lines = append(lines, OffsetOrRange{Start: line})
	}
	read := func(lookbackKB string) (compressed, decoded int64) {
		t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", lookbackKB)
		counter := withCountingOpen(t)
		frames := countDecodedFrames(t)
		resp, err := Resolve(t.Context(), Request{Path: path, Lines: lines, IndexLoader: NoIndex})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got := resp.LineTimestamps["5100"]; len(got) != 1 || (got[0] == nil) != (lookbackKB == "0") {
			t.Fatalf("lookback %s KiB: line_timestamps of line 5100: %v", lookbackKB, got)
		}
		return counter.Load(), frames.Load()
	}
	withoutCompressed, _ := read("0")
	withCompressed, decoded := read("64")
	if extra := withCompressed - withoutCompressed; extra > info.Size() {
		t.Errorf("the read back read %d compressed bytes of a %d-byte file", extra, info.Size())
	}
	if decoded > 2*frameText {
		t.Errorf("the read back decoded %d bytes; its keys lie in two frames of %d", decoded, frameText)
	}
}
