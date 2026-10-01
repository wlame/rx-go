package index

import (
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
