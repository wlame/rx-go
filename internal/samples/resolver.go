package samples

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// IndexLoader is the loose-coupling seam between this package and the
// unified index cache. The CLI and HTTP callers both construct a loader
// that hits internal/index.LoadForSource; tests substitute an in-memory
// stub. Returning (nil, nil) means "no index available — use a linear
// scan fallback". Returning an error aborts the resolve.
type IndexLoader func(path string) (*rxtypes.UnifiedFileIndex, error)

// NoIndex is an IndexLoader that always reports "no cache". Useful in
// tests and when callers explicitly want to skip index-aware seeks:
// `rx samples --no-index` and RX_NO_INDEX use it, so the lookup reads
// no index file at all.
func NoIndex(string) (*rxtypes.UnifiedFileIndex, error) { return nil, nil }

// StoredIndex is the IndexLoader `rx samples` and GET /v1/samples use:
// the line index stored for path when it still describes the file
// (index.LoadForSource), and none otherwise.
//
// It never returns an error. A missing, stale, unreadable or truncated
// index is an absent one (index.LoadFromPath logs the unreadable and
// truncated kinds), and the lookup reads the file without it: an index
// only makes the answer faster, so failing the lookup over one would
// trade the answer for the accelerator.
func StoredIndex(path string) (*rxtypes.UnifiedFileIndex, error) {
	idx, err := index.LoadForSource(path)
	if err != nil {
		return nil, nil //nolint:nilerr // an index that cannot be loaded is absent
	}
	return idx, nil
}

// Request is the input to Resolve. Exactly one of Offsets, Lines or
// Timestamps must be non-empty. Context / BeforeContext / AfterContext are
// caller-resolved (Resolve does not apply defaults).
type Request struct {
	// Path is the file as the caller named it; the response reports it
	// back unchanged.
	Path string
	// Source is Path pinned when the caller checked it (paths.Pin).
	// Every read of the file goes through it, so a path that leads to
	// another file by then is refused rather than read. When it is the
	// zero value, Resolve pins Path itself before the first read.
	Source  paths.Pinned
	Offsets []OffsetOrRange
	Lines   []OffsetOrRange
	// Timestamps are time queries, each answered with the line at that
	// time (or the lines of a range) through the lines machinery; see
	// resolveTimestamps. Each value is one query: a value is never split
	// on commas, which are part of some timestamp formats.
	Timestamps    []string
	BeforeContext int
	AfterContext  int
	// Kind is what Source is (filekind.Of), when the caller decided it
	// already (Classify). Nil means Resolve decides it from Source.
	Kind *filekind.Kind
	// IndexLoader is invoked once per Resolve call if Lines mode and
	// large-file shortcuts are needed. Set to NoIndex for the linear
	// scan fallback.
	IndexLoader IndexLoader

	// MaxLines is the most lines the answer may hold, counted over all
	// its samples as the lines are read: past it Resolve stops and
	// returns ErrTooManyLines. 0 is no limit, which the CLI uses; the
	// HTTP API sets RX_SAMPLES_MAX_LINES, so one request cannot make the
	// server hold an answer of any size.
	MaxLines int
	// MaxBytes is the most bytes of line text the answer may hold,
	// counted with MaxLines, the line breaks not included: past it
	// Resolve stops and returns ErrTooManyBytes. A line is never held
	// whole when it alone is longer. 0 is no limit, which the CLI uses;
	// the HTTP API sets RX_SAMPLES_MAX_BYTES, since samples returns
	// whole lines and a log's lines can be megabytes long.
	MaxBytes int64

	// ctx is the context Resolve was called with. Every pass over the
	// file reads through it (withContext), so a canceled request stops
	// reading at the next buffer it fills. It is a field rather than a
	// parameter of each helper because the helpers already take the
	// request.
	ctx context.Context
}

// context returns the context of the Resolve call, or a context that is
// never canceled for a request built without one (a test that calls a
// helper directly).
func (r Request) context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// Mode reports which dispatch path Resolve will take: OffsetsMode when
// Offsets is non-empty, TimestampsMode when Timestamps is, LinesMode
// otherwise.
func (r Request) Mode() Mode {
	switch {
	case len(r.Offsets) > 0:
		return OffsetsMode
	case len(r.Timestamps) > 0:
		return TimestampsMode
	}
	return LinesMode
}

// modeCount is how many of the three kinds of position r holds.
func (r Request) modeCount() int {
	count := 0
	for _, n := range []int{len(r.Offsets), len(r.Lines), len(r.Timestamps)} {
		if n > 0 {
			count++
		}
	}
	return count
}

// Mode is the enum of request dispatch paths.
type Mode int

// Mode values.
const (
	LinesMode Mode = iota
	OffsetsMode
	TimestampsMode
)

// Resolve executes the request and returns a populated SamplesResponse.
// Compression detection and the caller's error mapping stay with the
// caller, which also checks the path first to answer a refusal the way
// its surface does. Resolve reads the file only through req.Source, and
// pins req.Path itself (the same sandbox check) when the caller passed
// no pin, so it never reads a file that check would refuse.
//
// Offsets mode (byte offset → line):
//
//	Single:   key = string(start_byte), sample = ±context lines around
//	          the line containing start_byte, offsets[key] = line number.
//	Range:    key = "start-end", sample = every line overlapping the
//	          byte range [start, end], offsets[key] = start line number.
//
// Lines mode (line number → byte offset):
//
//	Single:   key = string(line), sample = ±context lines around line,
//	          lines[key] = byte offset of LINE (not context start).
//	Range:    key = "start-end", sample = lines start..end, lines[key]
//	          = -1 (sentinel; Python parity).
//
// Timestamps mode (time → line), see resolveTimestamps:
//
//	Single:   key = the query, sample = ±context lines around the first
//	          line whose own timestamp is at or after the time,
//	          timestamps[key] = that line's number.
//	Range:    key = the query, sample = the lines of the range,
//	          timestamps[key] = its first line.
//
// Negative single values are resolved against file size (byte mode) or
// total line count (lines mode). Ranges must have both ends >= 0.
//
// Every answer carries time_format, the file's timestamp format: from
// the index when there is one, else from the head of the text (at most
// a mebibyte), and line_timestamps, the effective timestamp of every
// sample line (see fileTimes.lineTimestamps).
//
// ctx bounds the work: when it is canceled (the HTTP client went
// away), the pass that is reading stops at its next read and Resolve
// returns ctx's error.
func Resolve(ctx context.Context, req Request) (*rxtypes.SamplesResponse, error) {
	req.ctx = ctx
	if req.modeCount() > 1 {
		return nil, ErrInvalidRequest
	}
	if req.Source.IsZero() {
		src, err := paths.Pin(req.Path)
		if err != nil {
			return nil, err
		}
		req.Source = src
	}
	req.IndexLoader = loadOnce(onlyIndexesOf(req.Source, req.IndexLoader))
	kind, err := Classify(req)
	if err != nil {
		return nil, err
	}
	resp := &rxtypes.SamplesResponse{
		Path:          req.Path,
		Offsets:       map[string]int64{},
		Lines:         map[string]int64{},
		BeforeContext: req.BeforeContext,
		AfterContext:  req.AfterContext,
		Samples:       map[string][]string{},
		Timestamps:    map[string]int64{},
	}
	answer := newCollected(resp, req)
	if kind.IsCompressed() {
		resp.IsCompressed = true
		name := kind.CompressionName()
		resp.CompressionFormat = &name
	}

	// Byte offsets take one path for every file: the offsets are
	// positions in the file's text, the decompressed stream for a
	// compressed file, and the text source decides how that text is
	// reached. Doing this here rather than in a caller is what keeps
	// `rx samples` and GET /v1/samples answering the same way.
	times, err := timesForMode(req, kind)
	if err != nil {
		return nil, err
	}
	resp.TimeFormat = times.describe()

	switch req.Mode() {
	case OffsetsMode:
		err = resolveOffsets(req, answer, textSourceFor(req, kind))
	case TimestampsMode:
		err = resolveTimestamps(req, kind, times, answer)
	default:
		err = resolveLineWindows(req, kind, answer)
	}
	if err != nil {
		return nil, err
	}
	if resp.LineTimestamps, err = times.lineTimestamps(req, kind, answer); err != nil {
		return nil, err
	}
	return resp, nil
}

// Classify decides what req.Source is (filekind.OfPinned), or returns
// req.Kind when the caller decided it already. A file that cannot be
// opened returns the open's error (fs.ErrPermission for one the process
// may not read). A file that is not text returns an error wrapping
// filekind.ErrNotText that says why: samples has no lines to give from
// a binary file, and reading its bytes as lines would answer garbage.
func Classify(req Request) (filekind.Kind, error) {
	if req.Kind != nil {
		return *req.Kind, req.Kind.Err()
	}
	kind, err := filekind.OfPinnedForReading(req.Source)
	if err != nil {
		return kind, err
	}
	return kind, kind.Err()
}

// onlyIndexesOf wraps load so that an index built from another file
// than src is reported as absent, (nil, nil).
//
// The loader looks the index up by path, and validates it against what
// the path leads to at that moment; the file itself is read through
// src. A link retargeted and put back around the look-up makes the two
// differ, and the other file's checkpoints would give wrong line
// numbers. A nil loader stays nil.
func onlyIndexesOf(src paths.Pinned, load IndexLoader) IndexLoader {
	if load == nil {
		return nil
	}
	return func(path string) (*rxtypes.UnifiedFileIndex, error) {
		idx, err := load(path)
		if err != nil || index.DescribesPinned(idx, src) {
			return idx, err
		}
		return nil, nil
	}
}

// loadOnce wraps load so that it is called at most once: every later
// call returns the first call's answer. One Resolve consults the index
// from several places (the text source, the time format, the lines
// machinery), and StoredIndex reads and parses the index file on each
// call. The wrapper serves one path, the request's; a nil loader stays
// nil.
func loadOnce(load IndexLoader) IndexLoader {
	if load == nil {
		return nil
	}
	var (
		called bool
		idx    *rxtypes.UnifiedFileIndex
		err    error
	)
	return func(path string) (*rxtypes.UnifiedFileIndex, error) {
		if !called {
			called = true
			idx, err = load(path)
		}
		return idx, err
	}
}

// loadIndexOrNone returns the request's index, or nil when the loader
// has none or cannot provide it: an index only makes the answer faster,
// so a missing or unreadable one costs time, never correctness.
func loadIndexOrNone(req Request) *rxtypes.UnifiedFileIndex {
	if req.IndexLoader == nil {
		return nil
	}
	idx, _ := req.IndexLoader(req.Path)
	return idx
}

// ============================================================================
// Byte-offset mode
// ============================================================================

// window is one byte-offset request being collected as the file goes by.
type window struct {
	key     string
	start   int64
	end     int64 // byte range end, or -1 for a single offset
	line    int64
	started bool
	done    bool
	after   int
	collect []string
	// starts holds where each line of collect starts in the text.
	starts []int64
}

// resolveOffsets answers every byte offset in the request from one
// sequential pass over the file's text.
//
// A caller with a batch of match offsets — which is how the viewer asks
// after a capped search — used to pay a full scan per offset, once to
// find the line and again to read the window around it. The pass here
// resolves the line numbers and collects the windows together, keeping
// the last few lines in a ring so a window that reaches backwards is
// already in hand.
//
// text says how the pass reaches the file's text: a plain file is read
// as it is, a compressed one through its decompressor, and either
// starts near the first offset when an index says where that is. The
// pass itself is the same for every file, which is what keeps a plain
// file and its compressed copies answering identically.
func resolveOffsets(req Request, resp *collected, text textSource) error {
	windows := make([]*window, 0, len(req.Offsets))
	var textSize int64 = -1
	for _, v := range req.Offsets {
		start := v.Start
		if start < 0 {
			// Python parity: a negative offset counts back from the end
			// and the response reports the resolved positive value.
			if textSize < 0 {
				size, err := text.size()
				if err != nil {
					return err
				}
				textSize = size
			}
			start = textSize + start
			if start < 0 {
				start = 0
			}
		}
		w := &window{key: strconv.FormatInt(start, 10), start: start, end: -1}
		if v.IsRange() {
			w.key, w.end = v.Key(), *v.End
		}
		windows = append(windows, w)
		resp.Samples[w.key] = []string{}
	}
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].start < windows[j].start })

	// The pass may start at a line before the first offset, as long as
	// it leaves room for the leading context.
	cursor, err := text.openNear(windows[0].start, req.BeforeContext)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.close() }()

	before := newLineRing(req.BeforeContext, req.MaxBytes)
	lines := newLineReader(withContext(req.context(), cursor), readBufferFor(windows[len(windows)-1].start-cursor.offset))
	pos, lineNum, next := cursor.offset, cursor.line, 0
	// active holds the started windows that still want lines, so a line
	// costs one step per window it belongs to rather than one per
	// window of the request.
	var active []*window
	for {
		// Every line may become context of a window that starts later,
		// so each is kept, but never more of it than the budget could
		// hold: a longer line is cut, and taking it fails before its
		// cut text is used.
		raw, length, ended, readErr := lines.readUpTo(resp.budget.keepPerLine())
		if readErr != nil {
			return readErr
		}
		if length == 0 {
			break
		}
		textBytes := textLength(raw, length)
		// A line cut to the limit is never filed (taking it fails), so
		// none of it is copied into a string or kept in the ring.
		var text string
		if int64(len(raw)) == length {
			text = string(trimOneLineBreak(raw))
		}
		end := pos + length

		// Start every window whose offset falls on this line.
		for next < len(windows) && windows[next].start < end {
			w := windows[next]
			w.started, w.line = true, lineNum
			resp.Offsets[w.key] = lineNum
			if w.end < 0 {
				context := before.lines()
				// The whole context is taken before any of it is used:
				// a line whose text the ring let go is in a context
				// that passes the byte limit (lineRing).
				for _, l := range context {
					if err := resp.budget.take(l.textBytes); err != nil {
						return err
					}
				}
				for _, l := range context {
					w.collect = append(w.collect, l.text)
					w.starts = append(w.starts, l.start)
				}
				w.after = req.AfterContext
			}
			active = append(active, w)
			next++
		}

		// Filtering into active[:0] reuses the slice's array: each kept
		// window is written at or before the place it is read from.
		kept := active[:0]
		for _, w := range active {
			if err := resp.budget.take(textBytes); err != nil {
				return err
			}
			w.collect = append(w.collect, text)
			w.starts = append(w.starts, pos)
			switch {
			case w.end >= 0:
				// A byte range ends on the line holding its end offset.
				if w.end < end {
					w.done = true
				}
			case lineNum > w.line:
				w.after--
				if w.after <= 0 {
					w.done = true
				}
			case req.AfterContext == 0:
				w.done = true
			}
			if !w.done {
				kept = append(kept, w)
			}
		}
		active = kept

		before.push(ringLine{text: text, start: pos, textBytes: textBytes})
		pos, lineNum = end, lineNum+1
		if ended {
			break
		}
		if next >= len(windows) && len(active) == 0 {
			break
		}
	}

	for _, w := range windows {
		if !w.started {
			// The offset is past the end of the file, so there is no
			// line at it. Reporting the file's last line was a number
			// counted from the wrong place; -1 is what the line
			// numbering contract spells "asked but unknown", and it is
			// what a line number past the last line already answers.
			resp.Offsets[w.key] = -1
			resp.Samples[w.key] = nil
			continue
		}
		resp.Samples[w.key] = w.collect
		resp.starts[w.key] = w.starts
	}
	return nil
}

// readBufferFor sizes the read buffer to the span a pass will cover.
//
// A pass over a large file wants a big buffer to keep the syscall count
// down; a request that stops a few kilobytes in wants a small one,
// because whatever the buffer holds past the answer is read for
// nothing. The bounded-read tests measure exactly that overshoot.
func readBufferFor(span int64) int {
	const smallBuffer, largeBuffer = 4 * 1024, 64 * 1024
	if span > 512*1024 {
		return largeBuffer
	}
	return smallBuffer
}

// checkpointBefore returns the index checkpoint to start a pass from so
// that `context` lines are available before `offset`, and the line
// number that checkpoint names: (0, 1) when the pass starts at the
// first byte. The checkpoint can be several checkpoints back, since on
// a log of long lines one checkpoint gap holds fewer lines than the
// context asks for (index.CheckpointForContext).
func checkpointBefore(
	idx *rxtypes.UnifiedFileIndex,
	offset int64,
	context int,
) (byteOffset, line int64) {
	entry := index.CheckpointForContext(idx, offset, context)
	if entry.LineNumber == 0 {
		return 0, 1
	}
	return entry.ByteOffset, entry.LineNumber
}

// lineRing remembers the last n lines read, which is what a window that
// reaches backwards needs.
//
// SECURITY: it keeps at most byteLimit bytes of their text (0: no
// limit). The lines are held before any window takes them from the
// answer's budget, so without this bound before_context lines of up to
// the byte limit each would be in memory at once. When a new line pushes
// the text past the limit, the oldest lines' text is let go; their
// sizes stay. A window that later starts includes, with such a line,
// every newer line that was in the ring when it was let go, so taking
// the window's context from the budget passes the limit before any of
// it is used (resolveOffsets takes the whole context first).
type lineRing struct {
	buf       []ringLine
	next      int
	size      int
	byteLimit int64
	keptBytes int64
}

// ringLine is one remembered line: its text and where it starts in the
// file's text.
type ringLine struct {
	text  string
	start int64
	// textBytes is the length of the line's text, which is longer than
	// text when the line was cut to the answer's byte limit.
	textBytes int64
}

// newLineRing returns a ring of the last n lines that keeps at most
// byteLimit bytes of their text (0: no limit).
func newLineRing(n int, byteLimit int64) *lineRing {
	if n < 0 {
		n = 0
	}
	return &lineRing{buf: make([]ringLine, n), byteLimit: byteLimit}
}

// push remembers line, forgetting the oldest line when the ring is full
// and the oldest lines' text when the text kept passes the byte limit.
func (r *lineRing) push(line ringLine) {
	if len(r.buf) == 0 {
		return
	}
	r.keptBytes -= int64(len(r.buf[r.next].text))
	r.buf[r.next] = line
	r.keptBytes += int64(len(line.text))
	r.next = (r.next + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
	oldest := (r.next - r.size + len(r.buf)) % len(r.buf)
	for i := 0; r.byteLimit > 0 && r.keptBytes > r.byteLimit && i < r.size; i++ {
		slot := &r.buf[(oldest+i)%len(r.buf)]
		r.keptBytes -= int64(len(slot.text))
		slot.text = ""
	}
}

// lines returns the remembered lines in file order, oldest first.
func (r *lineRing) lines() []ringLine {
	if r.size == 0 {
		return nil
	}
	out := make([]ringLine, 0, r.size)
	start := (r.next - r.size + len(r.buf)) % len(r.buf)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// ============================================================================
// Low-level file helpers (seeking, line counting)
// ============================================================================

// readSeekCloser is the narrow interface the line-reading helpers need.
// Extracted from *os.File so tests can inject counting wrappers that
// observe byte traffic without changing production signatures. Every
// method is a direct subset of *os.File's surface — no adapter needed
// at the call site.
type readSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// openFileForSamples is the indirection point tests hook to count
// bytes read from the source file. Production default is os.Open wrapped
// so the returned value satisfies readSeekCloser. Tests reassign this
// var to a function that wraps *os.File in counting.File, observe
// counter.Load() after the operation, and restore the original in
// t.Cleanup. See resolver_budget_test.go.
//
// Every file the package reads is opened here or through the same pin:
// Pinned.Open refuses a path that no longer leads to the checked file.
var openFileForSamples = func(src paths.Pinned) (readSeekCloser, error) {
	return src.Open()
}

// chooseSeekOrigin walks idx.LineIndex in reverse and returns the
// checkpoint at-or-before targetLine. Returns (0, 1) when no index is
// available — the caller scans from the top.
//
// The unified index stores [[line_number, byte_offset], ...] pairs so
// we can jump straight to the nearest checkpoint. Without it, a linear
// scan is still correct, just slower.
func chooseSeekOrigin(idx *rxtypes.UnifiedFileIndex, targetLine int64) (offset, line int64) {
	if idx == nil || len(idx.LineIndex) == 0 {
		return 0, 1
	}
	if entry := index.FindNearestCheckpoint(idx, targetLine); entry.LineNumber > 0 {
		return entry.ByteOffset, entry.LineNumber
	}
	return 0, 1
}

// countLines returns the number of '\n' bytes + 1 if the final chunk
// has unterminated content. Matches Python's `sum(1 for _ in open(p, 'rb'))`.
func countLines(ctx context.Context, src paths.Pinned) (int64, error) {
	f, err := src.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(withContext(ctx, f))
	var (
		total       int64
		tailHasData bool
	)
	buf := make([]byte, 64*1024)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			total += int64(bytes.Count(chunk, []byte{'\n'}))
			// If the last byte of the last chunk is NOT a newline,
			// there's a trailing unterminated line to count. gosec
			// needs the indexing to go through the slice bound it
			// already proved (`len(chunk) == n > 0`).
			tailHasData = chunk[len(chunk)-1] != '\n'
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
	}
	if tailHasData {
		total++
	}
	return total, nil
}

// FormatInt64 returns the JSON-compatible decimal form of n. Exposed
// so callers building response keys can avoid importing strconv just
// for this.
func FormatInt64(n int64) string { return strconv.FormatInt(n, 10) }

// ErrInvalidRequest is returned when a Request holds more than one of
// Offsets, Lines and Timestamps.
var ErrInvalidRequest = fmt.Errorf("samples.Resolve: at most one of Offsets, Lines and Timestamps may be set")
