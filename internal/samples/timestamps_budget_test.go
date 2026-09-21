package samples

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/testutil/counting"
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
	byTime, err := Resolve(Request{Path: path, Timestamps: []string{iso(target)}, BeforeContext: 3, AfterContext: 3, IndexLoader: loader})
	if err != nil {
		t.Fatalf("by time: %v", err)
	}
	timeRead := counter.Load()
	if got := byTime.Timestamps[iso(target)]; got != 17_347 {
		t.Fatalf("line %d, want 17347", got)
	}

	counter.Store(0)
	if _, err := Resolve(Request{Path: path, Lines: []OffsetOrRange{{Start: 17_347}}, BeforeContext: 3, AfterContext: 3, IndexLoader: loader}); err != nil {
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
	resp, err := Resolve(Request{Path: path, Lines: []OffsetOrRange{{Start: 1}}, IndexLoader: NoIndex})
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
	stamp, found, err := lastStampFromEnd(text, size, parserFor(t, textOf(lines)))
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
		switch i % 6 {
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
		// The read back from the end, from this line's start.
		start := int64(len(bytes.Join(lines[:i], nil)))
		window, err := lineWindow(bytes.NewReader(text), text[start:min(int64(len(text)), start+timestamps.WindowBytes+1)], start, int64(len(text)))
		if err != nil {
			t.Fatalf("line %d: window: %v", i+1, err)
		}
		content := bytes.TrimRight(line, "\r\n")
		if wantWindow := content[:min(len(content), timestamps.WindowBytes)]; !bytes.Equal(window, wantWindow) {
			t.Fatalf("line %d: window %q, want %q", i+1, window, wantWindow)
		}
	}
	if _, _, length, _ := reader.next(); length != 0 {
		t.Fatalf("a line after the last: %d bytes", length)
	}
	got, found, err := lastStampFromEnd(bytes.NewReader(text), int64(len(text)), parser)
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
	got, found, err := lastStampFromEnd(bytes.NewReader(text), int64(len(text)), parserFor(t, text[:70]))
	if err != nil || !found || got.Ms != timeBase+1000 {
		t.Fatalf("last stamp %+v %v %v, want 07:30:01", got, found, err)
	}
}
