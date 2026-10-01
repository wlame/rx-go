package sigscan

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/counting"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
)

// countingOpen returns an open seam that counts every byte the scan
// reads from the file.
func countingOpen(counter **counting.ReaderAt) func(paths.Pinned) (fileReader, int64, error) {
	return func(pin paths.Pinned) (fileReader, int64, error) {
		f, size, err := openPinned(pin)
		if err != nil {
			return nil, 0, err
		}
		*counter = counting.NewReaderAt(f)
		return readerAtCloser{ReaderAt: *counter, Closer: f}, size, nil
	}
}

// readerAtCloser joins a reader and the file it reads.
type readerAtCloser struct {
	io.ReaderAt
	io.Closer
}

// A scan of an N-byte file reads at most N bytes plus the
// chunk overlaps: the rest of the line at each chunk edge (and one
// small read past it), and the lines a block crossing an edge is walked
// again for.
func TestScan_ByteBudget(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	var b strings.Builder
	for b.Len() < 1<<20 {
		b.WriteString(randomText(uint64(b.Len()), 40))
		b.WriteString("\n")
	}
	text := []byte(b.String())
	file := writeFile(t, "app.log", text)

	var counter *counting.ReaderAt
	tune := defaultTuning()
	tune.chunkBytes, tune.readBytes, tune.edgeReadBytes = 64<<10, 4096, 256
	tune.open = countingOpen(&counter)
	res, err := scanFile(context.Background(), file, sigs, Options{Workers: 4, LineBytes: mixedLineBytes}, tune)
	if err != nil {
		t.Fatal(err)
	}
	// 16 edges; at each, the rest of a line (at most 1,103 bytes here)
	// read in 256-byte pieces by the chunk before, and a block walked
	// again (at most 5 lines of a block here, the traceback's 3 or the
	// BEGIN block's 5, plus the line that refuses it).
	edges := int64(len(text)) / tune.chunkBytes
	perEdge := int64(1103+256) + int64(6*1103+256)
	budget := int64(len(text)) + edges*perEdge
	if got := counter.Load(); got > budget || got < int64(len(text)) {
		t.Errorf("read %d bytes of a %d-byte file, budget %d", got, len(text), budget)
	}
	if res.BytesRead != counter.Load() {
		t.Errorf("BytesRead %d, the file was read for %d", res.BytesRead, counter.Load())
	}
}

// A stream is read once: the scan reads each byte of a gzip file once.
func TestScan_ByteBudgetOfAStream(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	path := filepath.Join(t.TempDir(), "app.log.gz")
	writeGzip(t, path, func(w io.Writer) {
		for i := range 2000 {
			_, _ = io.WriteString(w, randomText(uint64(i), 10)+"\n")
		}
	})
	var counter *counting.ReaderAt
	tune := defaultTuning()
	tune.batchLines = 100
	tune.open = countingOpen(&counter)
	if _, err := scanFile(context.Background(), classify(t, path), sigs, Options{Workers: 4, LineBytes: mixedLineBytes}, tune); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if got := counter.Load(); got != info.Size() {
		t.Errorf("read %d bytes of a %d-byte gzip file, want each byte once", got, info.Size())
	}
}

// writeGzip writes a gzip file whose text body writes, streaming, so a
// large text is never held in memory.
func writeGzip(t *testing.T, path string, body func(io.Writer)) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	zw := gzip.NewWriter(bw)
	body(zw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// writePlain writes a plain file whose text body writes, streaming.
func writePlain(t *testing.T, path string, body func(io.Writer)) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	body(bw)
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// peakHeapDuring runs fn and returns how far the live heap rose above
// what it was before, sampled every millisecond. The garbage collector
// runs often meanwhile (GOGC 10), so the peak follows what fn holds
// rather than what it threw away.
func peakHeapDuring(t *testing.T, fn func()) uint64 {
	t.Helper()
	previous := debug.SetGCPercent(10)
	defer debug.SetGCPercent(previous)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	// Go note: the sampler goroutine runs beside fn and stops when stop
	// is closed; wg.Wait makes sure it has stopped before the peak is
	// read.
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		var s runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&s)
				if s.HeapAlloc > base && s.HeapAlloc-base > peak.Load() {
					peak.Store(s.HeapAlloc - base)
				}
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return peak.Load()
}

// A 100 MB line is never held whole, in a plain file or a
// gzip stream: the scan holds its view and its length.
func TestScan_HundredMegabyteLineIsNotHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 100 MB")
	}
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	dir := t.TempDir()
	body := func(w io.Writer) {
		_, _ = io.WriteString(w, "ERROR code=1 before\n")
		piece := []byte(strings.Repeat("x", 1<<20))
		for range 100 {
			_, _ = w.Write(piece)
		}
		_, _ = io.WriteString(w, "\nERROR code=2 after\n")
	}
	plain, gz := filepath.Join(dir, "big.log"), filepath.Join(dir, "big.log.gz")
	writePlain(t, plain, body)
	writeGzip(t, gz, body)
	const bound = 32 << 20
	for _, path := range []string{plain, gz} {
		file := classify(t, path)
		var res *Result
		peak := peakHeapDuring(t, func() {
			var err error
			res, err = Scan(context.Background(), file, sigs, Options{Workers: 2, LineBytes: mixedLineBytes})
			if err != nil {
				t.Error(err)
			}
		})
		if res == nil {
			continue
		}
		if got := spans(res, "err-code"); len(got) != 2 || got[1].start != 3 {
			t.Errorf("%s: findings %v, want lines 1 and 3", filepath.Base(path), got)
		}
		if peak > bound {
			t.Errorf("%s: the heap rose by %d MiB while scanning a 100 MiB line, bound %d MiB", filepath.Base(path), peak>>20, bound>>20)
		}
	}
}

// A signature that matches every line of a 64 MB file,
// with a cap of 100: memory stays bounded (no list of every candidate or
// block start), 100 findings are stored, and the count is exact.
func TestScan_NoisySignatureKeepsMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 64 MB")
	}
	sigs := loadSigs(t, sigFile(
		regexSig("every-line", `LINE \d+`),
		kindSig("every-block", "block", `start: '^LINE'`, "lines: 1"),
	), "every-line", "every-block")
	path := filepath.Join(t.TempDir(), "noisy.log")
	lines := 0
	writePlain(t, path, func(w io.Writer) {
		for written := 0; written < 64<<20; lines++ {
			n, _ := fmt.Fprintf(w, "LINE %08d payload payload payload payload payload\n", lines)
			written += n
		}
	})
	file := classify(t, path)
	var res *Result
	peak := peakHeapDuring(t, func() {
		var err error
		res, err = Scan(context.Background(), file, sigs, Options{Workers: 4, Cap: 100})
		if err != nil {
			t.Error(err)
		}
	})
	if res == nil {
		return
	}
	for id, wantCount := range map[string]int64{"every-line": int64(lines), "every-block": int64(lines+1) / 2} {
		got := res.BySignature[id]
		if len(got.Findings) != 100 || got.Count != wantCount || got.Findings[99].StartLine >= got.Findings[0].StartLine+200 {
			t.Errorf("%s: stored %d (last at line %d), count %d; want 100 stored from the start, count %d",
				id, len(got.Findings), got.Findings[len(got.Findings)-1].StartLine, got.Count, wantCount)
		}
	}
	if peak > 64<<20 {
		t.Errorf("the heap rose by %d MiB while scanning a 64 MiB file", peak>>20)
	}
}

// A canceled context ends the scan with context.Canceled and
// no result, on a chunked file and on a stream.
func TestScan_CancelEndsTheScan(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	dir := t.TempDir()
	body := func(w io.Writer) {
		for i := range 4000 {
			_, _ = io.WriteString(w, randomText(uint64(i), 10)+"\n")
		}
	}
	plain, gz := filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.gz")
	writePlain(t, plain, body)
	writeGzip(t, gz, body)
	for _, path := range []string{plain, gz} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tune := defaultTuning()
			tune.chunkBytes, tune.batchLines = 4096, 50
			opts := Options{Workers: 3, Progress: func(read, total int64) { cancel() }}
			res, err := scanFile(ctx, classify(t, path), sigs, opts, tune)
			if !errors.Is(err, context.Canceled) || res != nil {
				t.Fatalf("got result %v and error %v, want context.Canceled and no result", res != nil, err)
			}
		})
	}
}

// truncatingOpen returns an open seam that cuts the file to half its
// size after the scan's first read: the copytruncate of a log rotation,
// in the middle of a scan.
func truncatingOpen(path string) func(paths.Pinned) (fileReader, int64, error) {
	return func(pin paths.Pinned) (fileReader, int64, error) {
		f, size, err := openPinned(pin)
		if err != nil {
			return nil, 0, err
		}
		return readerAtCloser{ReaderAt: &truncateAfterFirstRead{r: f, path: path, size: size}, Closer: f}, size, nil
	}
}

// truncateAfterFirstRead truncates the file once its first read is
// done.
type truncateAfterFirstRead struct {
	r    io.ReaderAt
	path string
	size int64
	once sync.Once
}

func (t *truncateAfterFirstRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := t.r.ReadAt(p, off)
	t.once.Do(func() { _ = os.Truncate(t.path, t.size/2) })
	return n, err
}

// A file truncated during the pass ends the scan
// with an error that names the file, never a panic or an offset past
// the new end; plain, gzip and seekable alike.
func TestScan_FileTruncatedDuringTheScan(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	var text strings.Builder
	for i := range 3000 {
		text.WriteString(randomText(uint64(i), 10) + "\n")
	}
	dir := t.TempDir()
	plain, gz, zst := filepath.Join(dir, "app.log"), filepath.Join(dir, "app.log.gz"), filepath.Join(dir, "app.log.zst")
	writePlain(t, plain, func(w io.Writer) { _, _ = io.WriteString(w, text.String()) })
	writeGzip(t, gz, func(w io.Writer) { _, _ = io.WriteString(w, text.String()) })
	seekablefile.Write(t, zst, seekablefile.SplitEvery([]byte(text.String()), 4096))
	for _, path := range []string{plain, gz, zst} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			file := classify(t, path)
			tune := defaultTuning()
			tune.chunkBytes, tune.readBytes, tune.batchBytes = 8192, 4096, 4096
			tune.open = truncatingOpen(path)
			res, err := scanFile(context.Background(), file, sigs, Options{Workers: 3, LineBytes: mixedLineBytes}, tune)
			if err == nil || res != nil {
				t.Fatalf("got a result (%v) and error %v, want an error and no result", res != nil, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the error does not name the file: %v", err)
			}
		})
	}
}

// An empty selection reads nothing.
func TestScan_EmptySelectionReadsNothing(t *testing.T) {
	var counter *counting.ReaderAt
	tune := defaultTuning()
	tune.open = countingOpen(&counter)
	res, err := scanFile(context.Background(), writeFile(t, "app.log", []byte("ERROR\n")), nil, Options{}, tune)
	if err != nil || len(res.BySignature) != 0 || counter != nil {
		t.Fatalf("result %v, error %v, file opened %v", res, err, counter != nil)
	}
}

// A cancel during a replay that never converges ends the scan within a
// few thousand lines of the replay, not after a whole chunk. Every line
// starts a two-line block, so a block carried across an odd chunk edge
// keeps the merge's walk and the chunk's own walk out of step to the
// end of the chunk.
func TestScan_CancelStopsALongReplay(t *testing.T) {
	sigs := loadSigs(t, sigFile(kindSig("pairs", "block", `start: '^L'`, "lines: 1")), "pairs")
	var b strings.Builder
	for i := range 60001 {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var replayed atomic.Int64
	tune := defaultTuning()
	tune.chunkLines = 20001
	tune.rewalkLine = func() {
		if replayed.Add(1) == 1 {
			cancel()
		}
	}
	res, err := scanFile(ctx, writeFile(t, "app.log", []byte(b.String())), sigs, Options{Workers: 2}, tune)
	if !errors.Is(err, context.Canceled) || res != nil {
		t.Fatalf("result %v, error %v, want context.Canceled", res != nil, err)
	}
	if n := replayed.Load(); n > replayCheckLines+1 {
		t.Errorf("the replay walked %d lines after the cancel, want at most %d", n, replayCheckLines+1)
	}
}

// shortLineText is n lines of at most about 100 bytes, with blocks of
// up to 5 lines and runs, from a fixed seed: every line fits in a
// 512-byte frame with room to spare.
func shortLineText(n int) string {
	r := rand.New(rand.NewPCG(3, 5))
	pieces := [][]string{
		{"INFO ok request handled"}, {"ERROR code=1 boom"}, {"ERROR code=2", "ERROR code=3"}, {"", "", ""},
		{"Traceback (most recent call last):", "  File \"a.py\", line 1", "ValueError: v"},
		{"BEGIN", "a", "b", "END"}, {"BEGIN", "1", "2", "3", "4", "5"}, {"HEAD", "h1"}, {"same", "same"},
	}
	var b strings.Builder
	for written := 0; written < n; {
		for _, l := range pieces[r.IntN(len(pieces))] {
			b.WriteString(l + "\n")
			written++
		}
	}
	return b.String()
}

// A seekable file is read once, plus at each chunk edge the frames the
// chunk before decodes to finish its last line and the frames the merge
// decodes again to finish a block crossing the edge: here at most 4
// frames an edge (a line spans at most 2 frames of 512 bytes; a block
// is at most 6 lines, under 600 bytes), far less than a chunk's 16
// frames.
func TestScan_ByteBudgetOfSeekable(t *testing.T) {
	sigs := loadSigs(t, mixedCatalogue, mixedIDs...)
	path := filepath.Join(t.TempDir(), "app.log.zst")
	seekablefile.Write(t, path, seekablefile.SplitEvery([]byte(shortLineText(20000)), 512))
	file := classify(t, path)
	table := file.Kind().Table
	var frames, largest int64
	for _, f := range table.Frames {
		frames += f.CompressedSize
		largest = max(largest, f.CompressedSize)
	}
	var counter *counting.ReaderAt
	tune := defaultTuning()
	tune.framesPerChunk = 16
	tune.open = countingOpen(&counter)
	res, err := scanFile(context.Background(), file, sigs, Options{Workers: 4, LineBytes: mixedLineBytes}, tune)
	if err != nil {
		t.Fatal(err)
	}
	edges := int64(table.NumFrames/16 + 1)
	budget := frames + edges*4*largest
	if got := counter.Load(); got > budget || got < frames {
		t.Errorf("read %d bytes of %d bytes of frames (%d edges, largest frame %d), budget %d", got, frames, edges, largest, budget)
	}
	if res.BySignature["begin-end"].Count == 0 {
		t.Error("the fixture has no block to carry across an edge")
	}
}
