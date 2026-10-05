package index

import (
	"context"
	"io"
	"sync/atomic"
)

// Progress counts how far a running Build has read, so that another
// goroutine (an HTTP status request, say) can report it while the build
// goes on.
//
// The build sets the total once it knows the size of what it will read,
// then records how far it has read. Both counters are atomics: the build
// goroutines write them and any number of reader goroutines read them,
// with no lock and no data race. The unit is whatever the build reads
// through: for a plain or stream-compressed file, the file's bytes, done
// being the highest byte of the file read so far (for a stream read from
// the start, every byte read); for a seekable zstd file, the decompressed
// text, whose frames are decoded one by one rather than read as a
// stream. Fraction hides the unit.
//
// The zero value is ready to use and reports no progress until the
// build starts.
type Progress struct {
	done  atomic.Int64
	total atomic.Int64
}

// Fraction returns the share of the build's input read so far, from 0
// to 1, and false while the build has not said how much there is.
func (p *Progress) Fraction() (float64, bool) {
	total := p.total.Load()
	if total <= 0 {
		return 0, false
	}
	fraction := float64(p.done.Load()) / float64(total)
	if fraction > 1 {
		fraction = 1
	}
	return fraction, true
}

// start records how much the build is going to read.
func (p *Progress) start(total int64) {
	p.total.Store(total)
}

// reach records that the build has read the file up to end (the offset
// after the last byte read): done becomes the larger of itself and end.
// A nil p does nothing.
//
// Go note: CompareAndSwap stores end only if done still holds the value
// just loaded, so two goroutines that read at once (a zstd decoder reads
// ahead in goroutines of its own) never move done back. The loop retries
// when another goroutine changed done in between.
func (p *Progress) reach(end int64) {
	if p == nil {
		return
	}
	for {
		done := p.done.Load()
		if end <= done || p.done.CompareAndSwap(done, end) {
			return
		}
	}
}

// countWrites returns w, counting into p every byte written through it.
// A nil p returns w unchanged.
func (p *Progress) countWrites(w io.Writer) io.Writer {
	if p == nil {
		return w
	}
	return &progressWriter{Writer: w, progress: p}
}

// progressWriter is an io.Writer that adds what it writes to a Progress.
//
// Go note: embedding io.Writer gives the struct a Write method; the one
// declared below takes its place and calls the embedded one.
type progressWriter struct {
	io.Writer
	progress *Progress
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.progress.done.Add(int64(n))
	return n, err
}

// readFile returns the build's file r as the build reads it by position:
// stopped by its context (BuildContext) before each read, and its
// Progress set to the highest byte read so far. A build with neither a
// Progress nor a context that can be canceled gets r unchanged.
func (o BuildOptions) readFile(r io.ReaderAt) io.ReaderAt {
	ctx := o.ctx
	if ctx != nil && ctx.Done() == nil {
		// A context that can never be canceled needs no check.
		ctx = nil
	}
	if ctx == nil && o.Progress == nil {
		return r
	}
	return &buildFile{ReaderAt: r, ctx: ctx, progress: o.Progress}
}

// buildFile is the file of a build, read by position: each ReadAt first
// checks the build's context, then records how far it read.
//
// Go note: ReadAt may be called from several goroutines at once (a zstd
// decoder reads ahead in goroutines of its own); ctx.Err and the atomic
// Progress are safe for that.
type buildFile struct {
	io.ReaderAt
	ctx      context.Context // nil when nothing cancels the build
	progress *Progress       // nil when nobody follows the build
}

// ReadAt fails with the context's error once the build is canceled,
// before reading anything; otherwise it reads and moves the progress to
// the end of what it read.
func (f *buildFile) ReadAt(p []byte, off int64) (int, error) {
	if f.ctx != nil {
		if err := f.ctx.Err(); err != nil {
			return 0, err
		}
	}
	n, err := f.ReaderAt.ReadAt(p, off)
	f.progress.reach(off + int64(n))
	return n, err
}

// writeText returns w as the frames of a seekable file are decoded into
// it: counted into the build's Progress and stopped by its context.
func (o BuildOptions) writeText(w io.Writer) io.Writer {
	return stopWrites(o.ctx, o.Progress.countWrites(w))
}

// stopReads returns r stopped by ctx: once ctx is canceled, every Read
// fails with ctx's error before reading anything. A nil ctx, or one that
// can never be canceled (Done is nil), returns r unchanged, so a build
// without a cancel pays nothing.
func stopReads(ctx context.Context, r io.Reader) io.Reader {
	if ctx == nil || ctx.Done() == nil {
		return r
	}
	return &stoppedReader{Reader: r, ctx: ctx}
}

// stopWrites is stopReads for a writer.
func stopWrites(ctx context.Context, w io.Writer) io.Writer {
	if ctx == nil || ctx.Done() == nil {
		return w
	}
	return &stoppedWriter{Writer: w, ctx: ctx}
}

// stoppedReader is an io.Reader that fails once its context is
// canceled. The check runs once per Read, which reads a whole buffer, so
// it costs little beside the work on that buffer.
type stoppedReader struct {
	io.Reader
	ctx context.Context
}

func (r *stoppedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// stoppedWriter is an io.Writer that fails once its context is
// canceled; the frame decoder stops at the failed write.
type stoppedWriter struct {
	io.Writer
	ctx context.Context
}

func (w *stoppedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}
