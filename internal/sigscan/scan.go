package sigscan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wlame/rx-go/internal/compression"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/seekable"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
)

// maxWorkers bounds the walking goroutines of one scan, as RX_WORKERS
// is bounded: each holds a read buffer of tuning.readBytes.
const maxWorkers = 256

// tuning holds the sizes the pass is planned with. Production uses
// defaultTuning; tests shrink chunks and batches to put edges anywhere.
type tuning struct {
	// chunksPerWorker and minChunkBytes size the chunks of a plain or
	// seekable file: about chunksPerWorker per worker, each at least
	// minChunkBytes of text.
	chunksPerWorker int
	minChunkBytes   int64
	// readBytes is one read of a chunk; edgeReadBytes one read past a
	// chunk's end, where only the rest of its last line is needed.
	readBytes, edgeReadBytes int
	// batchBytes is the buffer of one batch of a stream.
	batchBytes int
	// In-window factor: how many chunks or batches per worker may be
	// read and not yet merged. It bounds the findings held in memory.
	inFlightPerWorker int

	// Test knobs, 0 when unused: chunkBytes sets the nominal chunk
	// size; chunkLines cuts a plain file every chunkLines lines;
	// framesPerChunk groups that many frames of a seekable file;
	// batchLines cuts a stream batch after that many lines.
	chunkBytes     int64
	chunkLines     int
	framesPerChunk int
	batchLines     int
	// gapLimit sets how many candidates a time-gap keeper holds after
	// it compacts; 0 means max(Cap, maxGapCandidates).
	gapLimit int
	// bodyCap sets how many bodies a body recorder keeps; 0 means
	// MaxBodiesPerSignature.
	bodyCap int
	// open opens the file; nil means its pin's Open.
	open func(paths.Pinned) (fileReader, int64, error)
	// rewalkLine, when set, is called for each line the merge walks
	// again (tests).
	rewalkLine func()
}

// gapLimitFor is the time-gap keeper limit of a scan whose cap is cap.
func (t tuning) gapLimitFor(cap int) int {
	if t.gapLimit > 0 {
		return t.gapLimit
	}
	return max(cap, maxGapCandidates)
}

// defaultTuning is what Scan uses.
func defaultTuning() tuning {
	return tuning{chunksPerWorker: 8, minChunkBytes: 4 << 20, readBytes: 4 << 20, edgeReadBytes: 64 << 10,
		batchBytes: 4 << 20, inFlightPerWorker: 2}
}

// fileReader is how a scan reads the file: by position, from several
// goroutines at once, which an *os.File allows.
type fileReader interface {
	io.ReaderAt
	io.Closer
}

// openPinned opens the file through its pin and takes its size now: the
// scan reads that many bytes, so a file that grows meanwhile is read as
// it was, and one that shrinks fails the scan.
func openPinned(pin paths.Pinned) (fileReader, int64, error) {
	f, err := pin.Open()
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Scan reads the text of file once and returns the findings of sigs,
// by signature id. file is the pinned, classified file
// trace.ClassifyForSearch returns: every read goes through its pin, and
// its Kind decides how the text is read (plain, seekable zstd, or a
// stream decoded by one goroutine).
//
// The answer is the same for every number of workers, every chunk size
// and every storage of the same text. A signature id given twice is
// scanned once. Every redacting block and run of sigs and of
// opts.Redactors is walked for its bodies, which give the result's
// MaskedRanges; an empty selection reads nothing.
//
// Canceling ctx stops every goroutine of the scan, and Scan returns
// ctx.Err() and no result. Any other failure (the file cannot be read,
// is damaged, or got shorter while it was read) returns an error that
// names the file, and no result.
func Scan(ctx context.Context, file trace.SearchFile, sigs []*signatures.Signature, opts Options) (*Result, error) {
	return scanFile(ctx, file, sigs, opts, defaultTuning())
}

// scanFile is Scan with the sizes given.
func scanFile(ctx context.Context, file trace.SearchFile, sigs []*signatures.Signature, opts Options, tune tuning) (*Result, error) {
	path, kind := file.Path(), file.Kind()
	if !kind.IsText() {
		return nil, fmt.Errorf("%s: %w%s", path, filekind.ErrNotText, strings.TrimPrefix(kind.NotText, filekind.NotTextPrefix))
	}
	e, err := compile(sigs, opts)
	if err != nil {
		return nil, err
	}
	if len(e.sigs) == 0 {
		// Nothing to look for: the file is not read.
		return &Result{BySignature: map[string]*SignatureResult{}, MaskedRanges: [][2]int64{}}, nil
	}
	open := tune.open
	if open == nil {
		open = openPinned
	}
	fr, size, err := open(file.Pinned())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Go note: defer runs when scanFile returns, on every path, so the
	// file is closed whatever happens below.
	defer func() { _ = fr.Close() }()
	r := &countingReaderAt{r: fr}
	// The timestamp format is read from the head of the text through
	// the same counting reader, so BytesRead includes the head read.
	e.times, err = detectTimes(r, size, kind, mtimeOf(file.Pinned()), opts.LookbackBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	e.gapLimit = tune.gapLimitFor(e.cap)
	if tune.bodyCap > 0 {
		e.bodyCap = tune.bodyCap
	}
	s := newScanRun(e, opts, tune, size)
	var m *merger
	switch {
	case kind.IsSeekable():
		m, err = s.runSeekable(ctx, r, kind.Table)
	case kind.IsCompressed():
		m, err = s.runStream(ctx, r, size, kind.Format)
	default:
		m, err = s.runPlain(ctx, r, size)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.progress.finish()
	return &Result{BySignature: m.result(), MaskedRanges: m.ranges, BytesRead: r.n.Load()}, nil
}

// mtimeOf is the modification time of a pinned file, in nanoseconds
// since the Unix epoch, as the pin saw it (the time an index of the file
// records); 0 for a pin without file information.
func mtimeOf(pin paths.Pinned) int64 {
	info := pin.Info()
	if info == nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// scanBytes scans text held in memory as a plain file, whose mtime is
// taken as the Unix epoch.
func scanBytes(ctx context.Context, text []byte, sigs []*signatures.Signature, opts Options) (*Result, error) {
	e, err := compile(sigs, opts)
	if err != nil {
		return nil, err
	}
	e.times, err = detectTimes(bytes.NewReader(text), int64(len(text)), filekind.Kind{}, 0, opts.LookbackBytes)
	if err != nil {
		return nil, err
	}
	e.gapLimit = defaultTuning().gapLimitFor(e.cap)
	s := newScanRun(e, opts, defaultTuning(), int64(len(text)))
	m, err := s.runPlain(ctx, bytes.NewReader(text), int64(len(text)))
	if err != nil {
		return nil, err
	}
	return &Result{BySignature: m.result(), MaskedRanges: m.ranges}, nil
}

// countingReaderAt counts the bytes read through it, from any number of
// goroutines (an atomic counter).
type countingReaderAt struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

// progress reports the bytes read to Options.Progress. Workers call it
// from several goroutines; the mutex makes the calls one at a time, in
// increasing order.
type progress struct {
	mu          sync.Mutex
	fn          func(read, total int64)
	read, total int64
}

func (p *progress) add(n int64) {
	if p == nil || p.fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read = min(p.read+n, p.total)
	p.fn(p.read, p.total)
}

// set reports read bytes in all (the stream path counts them itself).
func (p *progress) set(read int64) {
	if p == nil || p.fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if read > p.read {
		p.read = min(read, p.total)
		p.fn(p.read, p.total)
	}
}

// finish reports the whole file read.
func (p *progress) finish() { p.set(p.total) }

// scanRun is one scan in progress.
type scanRun struct {
	e        *engine
	tune     tuning
	workers  int
	progress *progress
}

func newScanRun(e *engine, opts Options, tune tuning, size int64) *scanRun {
	workers := min(max(workerCount(opts), 1), maxWorkers)
	return &scanRun{e: e, tune: tune, workers: workers, progress: &progress{fn: opts.Progress, total: size}}
}

// chunkTarget is the nominal size of a chunk of text bytes: about
// chunksPerWorker chunks per worker, at least minChunkBytes each.
func (s *scanRun) chunkTarget(text int64) int64 {
	if s.tune.chunkBytes > 0 {
		return s.tune.chunkBytes
	}
	perChunk := (text + int64(s.tune.chunksPerWorker*s.workers) - 1) / int64(s.tune.chunksPerWorker*s.workers)
	return max(perChunk, s.tune.minChunkBytes)
}

// chunkSource opens cursors over a chunked file's text.
type chunkSource interface {
	// walkCursor reads a chunk for its walk, reusing the worker's read
	// buffer buf; done releases it.
	walkCursor(c chunkPlan, buf *[]byte) (cur textCursor, done func())
	// rewalkCursor reads a chunk's lines again from their start.
	rewalkCursor(span chunkSpan) (cur textCursor, done func())
}

// runPlain scans a plain file in chunks.
func (s *scanRun) runPlain(ctx context.Context, r io.ReaderAt, size int64) (*merger, error) {
	plans, err := s.planPlain(r, size)
	if err != nil {
		return nil, err
	}
	return s.runChunks(ctx, plans, &plainSource{r: r, size: size, tune: s.tune, progress: s.progress})
}

// planPlain cuts a plain file into chunks of chunkTarget bytes.
func (s *scanRun) planPlain(r io.ReaderAt, size int64) ([]chunkPlan, error) {
	if s.tune.chunkLines > 0 {
		return planEveryLines(r, size, s.tune.chunkLines)
	}
	target := s.chunkTarget(size)
	n := max((size+target-1)/target, 1)
	plans := make([]chunkPlan, n)
	for k := range n {
		plans[k] = chunkPlan{seq: int(k), from: k * size / n, to: (k + 1) * size / n}
	}
	return plans, nil
}

// planEveryLines cuts a plain file after every n lines (tests): each
// nominal boundary is the '\n' that ends a chunk's last line, so the
// next chunk starts on the line after it.
func planEveryLines(r io.ReaderAt, size int64, n int) ([]chunkPlan, error) {
	var bounds []int64
	lines := 0
	cur := &plainCursor{r: r, size: size, edge: size, readBytes: 64 << 10}
	pos := int64(0)
	for {
		piece, err := cur.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for i, c := range piece {
			if c == '\n' {
				lines++
				if lines%n == 0 && pos+int64(i)+1 < size {
					bounds = append(bounds, pos+int64(i))
				}
			}
		}
		pos += int64(len(piece))
	}
	plans := make([]chunkPlan, 0, len(bounds)+1)
	from := int64(0)
	for _, b := range bounds {
		plans = append(plans, chunkPlan{seq: len(plans), from: from, to: b})
		from = b
	}
	return append(plans, chunkPlan{seq: len(plans), from: from, to: size}), nil
}

// plainSource reads a plain file's chunks.
type plainSource struct {
	r        io.ReaderAt
	size     int64
	tune     tuning
	progress *progress
}

func (p *plainSource) walkCursor(c chunkPlan, buf *[]byte) (textCursor, func()) {
	cur := &plainCursor{r: p.r, pos: c.from, size: p.size, edge: c.to, readBytes: p.tune.readBytes,
		edgeReadBytes: p.tune.edgeReadBytes, progressTo: c.to, progress: p.progress, buf: *buf}
	// The worker keeps the buffer the cursor grew for its next chunk.
	return cur, func() { *buf = cur.buf }
}

func (p *plainSource) rewalkCursor(span chunkSpan) (textCursor, func()) {
	return &plainCursor{r: p.r, pos: span.start, size: p.size, edge: span.start, readBytes: p.tune.edgeReadBytes,
		edgeReadBytes: p.tune.edgeReadBytes}, func() {}
}

// runSeekable scans a seekable zstd file in chunks of whole frames.
func (s *scanRun) runSeekable(ctx context.Context, r io.ReaderAt, table *seekable.SeekTable) (*merger, error) {
	return s.runChunks(ctx, s.planSeekable(table), &seekableSource{r: r, table: table, progress: s.progress})
}

// planSeekable groups consecutive frames into chunks of about
// chunkTarget bytes of text.
func (s *scanRun) planSeekable(table *seekable.SeekTable) []chunkPlan {
	if table.NumFrames == 0 {
		return []chunkPlan{{}}
	}
	text := table.Frames[table.NumFrames-1].DecompressedEnd()
	target := s.chunkTarget(text)
	var plans []chunkPlan
	for a := 0; a < table.NumFrames; {
		b, size := a, int64(0)
		for b < table.NumFrames && (b == a || size < target) {
			if s.tune.framesPerChunk > 0 && b-a == s.tune.framesPerChunk {
				break
			}
			size += table.Frames[b].DecompressedSize
			b++
		}
		plans = append(plans, chunkPlan{seq: len(plans), from: table.Frames[a].DecompressedOffset,
			to: table.Frames[b-1].DecompressedEnd(), frame: a, endFrame: b})
		a = b
	}
	return plans
}

// seekableSource reads a seekable file's chunks, frame by frame.
type seekableSource struct {
	r        io.ReaderAt
	table    *seekable.SeekTable
	progress *progress
}

func (p *seekableSource) walkCursor(c chunkPlan, buf *[]byte) (textCursor, func()) {
	cur := newSeekableCursor(p.r, p.table, c.from, c.endFrame, p.progress)
	cur.comp = *buf
	return cur, func() {
		*buf = cur.comp
		cur.close()
	}
}

func (p *seekableSource) rewalkCursor(span chunkSpan) (textCursor, func()) {
	cur := newSeekableCursor(p.r, p.table, span.start, -1, nil)
	return cur, cur.close
}

// outcome is a chunk or batch result as a worker sends it.
type outcome struct {
	seq int
	res *chunkResult
	err error
}

// runChunks walks planned chunks in parallel and merges them in order.
//
// Three kinds of goroutine work here:
//   - one dispatcher sends the plans to the work channel, in order,
//     taking a token from window for each, so at most window's capacity
//     of chunks are being read or waiting for the merge at once;
//   - the workers take plans from work, walk them and send results;
//   - this goroutine merges the results in plan order (a result that
//     arrives early waits in pending) and gives a window token back for
//     each chunk merged.
//
// The first error (or a canceled ctx) cancels the derived context:
// the dispatcher stops, the workers stop between reads, and every
// goroutine has returned (wg.Wait) before runChunks does.
func (s *scanRun) runChunks(ctx context.Context, plans []chunkPlan, src chunkSource) (*merger, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(s.workers, len(plans))
	window := make(chan struct{}, s.tune.inFlightPerWorker*workers)
	work := make(chan chunkPlan)
	results := make(chan outcome, workers)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(work) // ends the workers' range loops
		for _, c := range plans {
			select {
			case window <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case work <- c:
			case <-ctx.Done():
				return
			}
		}
	}()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gs := s.e.gate.newScratch()
			var buf []byte // the worker's read buffer, reused chunk after chunk
			for c := range work {
				res, err := s.walkOne(ctx, src, c, gs, &buf)
				select {
				case results <- outcome{seq: c.seq, res: res, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	m := newMerger(ctx, s.e, s.tune.rewalkLine, func(res *chunkResult, fn func(*line) bool) error {
		cur, done := src.rewalkCursor(res.span)
		defer done()
		return walkFrom(cur, res.span.start, res.span.limit, s.e.lineBytes, fn)
	})
	err := mergeInOrder(ctx, results, nil, len(plans), m, func(*chunkResult) { <-window })
	cancel()
	wg.Wait()
	if err != nil {
		return nil, err
	}
	m.finish()
	return m, nil
}

// walkOne walks one chunk.
func (s *scanRun) walkOne(ctx context.Context, src chunkSource, c chunkPlan, gs *gateScratch, buf *[]byte) (*chunkResult, error) {
	w := newWalk(s.e, gs)
	cur, done := src.walkCursor(c, buf)
	defer done()
	start, final, err := walkChunk(ctx, cur, c, s.e.lineBytes, w.line)
	if err != nil {
		return nil, err
	}
	res := w.finish(final)
	res.seq, res.span = c.seq, chunkSpan{start: start, limit: c.to}
	return res, nil
}

// decodeEnd is how the decoder's run ended: the number of batches it
// sent, and its error.
type decodeEnd struct {
	batches int
	err     error
}

// mergeInOrder merges results in sequence order until total results
// are merged. total is known for chunks; for a stream it is -1 until
// the decoder reports it on ended. release runs after each merge.
func mergeInOrder(ctx context.Context, results <-chan outcome, ended <-chan decodeEnd, total int, m *merger, release func(*chunkResult)) error {
	pending := map[int]*chunkResult{}
	next := 0
	for total < 0 || next < total {
		select {
		case out := <-results:
			if out.err != nil {
				return out.err
			}
			pending[out.seq] = out.res
		case end := <-ended:
			if end.err != nil {
				return end.err
			}
			total, ended = end.batches, nil // a nil channel is never selected again
		case <-ctx.Done():
			return ctx.Err()
		}
		for {
			res, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			err := m.add(res)
			release(res)
			if err != nil {
				return err
			}
			next++
		}
	}
	return nil
}

// runStream scans a stream-compressed file: one decoder goroutine,
// workers on its batches, the merge in batch order.
//
// The goroutines:
//   - the decoder decompresses the file and sends batches on batches,
//     in order; it waits for a free buffer when maxBufs batches are
//     decoded and not yet merged, which bounds the memory of the
//     pipeline; when the stream ends it closes batches and reports on
//     ended;
//   - the workers walk batches and send results;
//   - this goroutine merges in batch order and gives each batch's
//     buffer back to the decoder.
func (s *scanRun) runStream(ctx context.Context, r io.ReaderAt, size int64, format compression.Format) (*merger, error) {
	counter, _ := r.(*countingReaderAt)
	rc, err := compression.NewReader(io.NopCloser(io.NewSectionReader(r, 0, size)), format)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := s.workers
	maxBufs := s.tune.inFlightPerWorker*workers + 2
	dec := &decoder{r: rc, lineBytes: s.e.lineBytes, batchBytes: s.tune.batchBytes, batchLines: s.tune.batchLines,
		free: make(chan []byte, maxBufs), maxBufs: maxBufs}
	if counter != nil {
		dec.onRead = func() { s.progress.set(counter.n.Load()) }
	}
	batches := make(chan *batch, workers)
	results := make(chan outcome, workers)
	ended := make(chan decodeEnd, 1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		n, decodeErr := dec.run(ctx, batches)
		close(batches)
		ended <- decodeEnd{batches: n, err: decodeErr}
	}()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gs := s.e.gate.newScratch()
			for b := range batches {
				w := newWalk(s.e, gs)
				walkBatch(b, s.e.lineBytes, w.line)
				res := w.finish(b.final)
				res.seq, res.span = b.seq, chunkSpan{batch: b}
				select {
				case results <- outcome{seq: b.seq, res: res}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	m := newMerger(ctx, s.e, s.tune.rewalkLine, func(res *chunkResult, fn func(*line) bool) error {
		walkBatch(res.span.batch, s.e.lineBytes, fn)
		return nil
	})
	err = mergeInOrder(ctx, results, ended, -1, m, func(res *chunkResult) {
		// The free channel holds every buffer ever made, so this send
		// never blocks.
		dec.free <- res.span.batch.buf
	})
	cancel()
	wg.Wait()
	if err != nil {
		return nil, err
	}
	m.finish()
	return m, nil
}
