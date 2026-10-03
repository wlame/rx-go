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
// then adds every byte it reads. Both counters are atomics: the build
// goroutine writes them and any number of reader goroutines read them,
// with no lock and no data race. The unit is whatever the build reads
// through: the file's bytes for a plain or stream-compressed file, the
// decompressed text for a seekable zstd file, whose frames are decoded
// one by one rather than read as a stream. Fraction hides the unit.
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

// countReads returns r, counting into p every byte read through it.
// A nil p returns r unchanged, so a build without a Progress pays
// nothing.
func (p *Progress) countReads(r io.Reader) io.Reader {
	if p == nil {
		return r
	}
	return &progressReader{Reader: r, progress: p}
}

// countWrites returns w, counting into p every byte written through it.
// A nil p returns w unchanged.
func (p *Progress) countWrites(w io.Writer) io.Writer {
	if p == nil {
		return w
	}
	return &progressWriter{Writer: w, progress: p}
}

// progressReader is an io.Reader that adds what it reads to a Progress.
//
// Go note: embedding io.Reader gives the struct a Read method; the one
// declared below takes its place and calls the embedded one.
type progressReader struct {
	io.Reader
	progress *Progress
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.progress.done.Add(int64(n))
	return n, err
}

// progressWriter is an io.Writer that adds what it writes to a Progress.
type progressWriter struct {
	io.Writer
	progress *Progress
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.progress.done.Add(int64(n))
	return n, err
}

// readText returns r as a build reads its text through it: counted into
// the build's Progress and stopped by its context (BuildContext).
func (o BuildOptions) readText(r io.Reader) io.Reader {
	return stopReads(o.ctx, o.Progress.countReads(r))
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
