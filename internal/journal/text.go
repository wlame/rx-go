package journal

// This file holds TextReader, the io.Reader of a journal's text: the
// entries of its entry list rendered one by one, each as journalctl
// prints it, the damaged ones left out and counted.

import (
	"errors"
	"io"

	"github.com/wlame/rx-go/internal/compression"
)

// Report counts what a full read of the text found.
// Entries == Shown + WithoutMessage + LeftOut + NotReached.
type Report struct {
	// Entries is the number of entries the header counts. Shown counts
	// the entries in the text, WithoutMessage the entries that have no
	// MESSAGE field and are not shown, LeftOut the damaged entries left
	// out, NotReached the entries after the place where the entry list
	// broke off.
	Entries, Shown, WithoutMessage, LeftOut, NotReached int64
	FirstDamaged                                        int64  // 1-based entry number in list order; 0 when none
	DamageReason                                        string // the first damage's reason; "" when none
}

// Damaged reports whether the read left out or did not reach an entry.
func (r Report) Damaged() bool { return r.LeftOut > 0 || r.NotReached > 0 }

// Options tunes a TextReader.
type Options struct {
	CacheBytes int64 // DATA cache bound; 0 means the default (32 MiB), below 0 no cache
}

// cacheBytes is the DATA cache's bound.
func (o Options) cacheBytes() int64 {
	if o.CacheBytes == 0 {
		return defaultCacheBytes
	}
	return o.CacheBytes
}

// maxEntryText bounds the text of one entry: 256 MiB, twice the largest
// value. An entry whose text could be longer (a message of many lines
// under a long prefix, each line indented by the prefix's length) is
// left out before any room is made for it.
const maxEntryText = 2 * compression.WindowLimit

// tooLongReason is the reason of an entry whose text could pass
// maxEntryText.
const tooLongReason = "renders to more than 256 MiB of text"

// keptBufferBytes is the largest buffer the reader keeps from one entry
// to the next; a larger one, made for a long entry, is let go.
const keptBufferBytes = 1 << 20

// errClosed is Read's error after Close.
var errClosed = errors.New("read of a closed journal text reader")

// TextReader is an io.Reader of a journal's text.
//
// It renders the entries one at a time into an internal buffer and hands
// the buffer out through Read, so it holds the text of one entry, the
// DATA cache and one decoded value at a time, never the whole text. It
// reads the file only through ReadAt and never closes it. A TextReader is
// used by one goroutine at a time.
type TextReader struct {
	walk   *walker
	report Report
	// buf holds the text of the last entry rendered; off is how much of
	// it Read has handed out.
	buf []byte
	off int
	// done is set when the walk is over; err holds a read error, which
	// every later Read returns.
	done   bool
	err    error
	closed bool
}

// NewTextReader returns a reader of the text of the journal r, a file of
// size bytes. It reads the header first: the error of a header rx cannot
// read wraps ErrUnreadable. The caller keeps r and closes it after the
// TextReader.
//
// Damage is not an error: Read leaves out what it cannot read and counts
// it in the Report. Read returns an error only when r.ReadAt fails for
// another reason than the end of the file, such as a disk error or a
// cancelled read, wrapped with %w.
func NewTextReader(r io.ReaderAt, size int64, opts Options) (*TextReader, error) {
	h, err := ReadHeader(r, size)
	if err != nil {
		return nil, err
	}
	w := newWalker(r, h, uint64(size), opts.cacheBytes()) // #nosec G115 -- ReadHeader accepted size, so it is at least 208
	return &TextReader{walk: w, report: Report{Entries: w.total}}, nil
}

// Read copies the next bytes of the text into p. It returns io.EOF after
// the last entry.
//
// Go note: having this method makes a *TextReader an io.Reader, so it
// works with io.Copy, bufio.Reader and every other consumer of a stream.
func (t *TextReader) Read(p []byte) (int, error) {
	if t.closed {
		return 0, errClosed
	}
	n := 0
	for n < len(p) {
		if t.off < len(t.buf) {
			copied := copy(p[n:], t.buf[t.off:])
			t.off += copied
			n += copied
			continue
		}
		if t.done || t.err != nil {
			break
		}
		t.err = t.readEntry()
	}
	switch {
	case n > 0:
		return n, nil
	case t.err != nil:
		return 0, t.err
	case t.done:
		return 0, io.EOF
	}
	return 0, nil
}

// readEntry takes the next entry of the list and, when it is shown,
// renders its text into t.buf; it counts the entry in the report either
// way. At the end of the walk it sets t.done.
func (t *TextReader) readEntry() error {
	t.buf, t.off = t.buf[:0], 0
	if cap(t.buf) > keptBufferBytes {
		t.buf = nil
	}
	offset, err := t.walk.next()
	if errors.Is(err, errListDone) {
		t.done = true
		return nil
	}
	if reason, ok := damageReason(err); ok {
		t.notReached(reason)
		return nil
	}
	if err != nil {
		return err
	}
	e, err := t.walk.readEntry(offset)
	if reason, ok := damageReason(err); ok {
		t.leftOut(reason)
		return nil
	}
	if err != nil {
		return err
	}
	// The walk checked the entry's realtime, so the renderer has a valid
	// time for it, and an entry it does not show is one without MESSAGE.
	room, shown := textRoom(e)
	switch {
	case !shown:
		t.report.WithoutMessage++
	case room > maxEntryText:
		t.leftOut(tooLongReason)
	default:
		t.buf, _ = AppendText(t.buf, e)
		t.report.Shown++
	}
	return nil
}

// leftOut counts the entry just taken from the list as damaged.
func (t *TextReader) leftOut(reason string) {
	t.report.LeftOut++
	t.firstDamage(t.walk.taken, reason)
}

// notReached ends the walk where the list broke off: every entry the
// header counts and the walk did not take is not reached.
func (t *TextReader) notReached(reason string) {
	t.done = true
	t.report.NotReached = t.report.Entries - t.walk.taken
	t.firstDamage(t.walk.taken+1, reason)
}

// firstDamage records the first damaged entry and its reason.
func (t *TextReader) firstDamage(entry int64, reason string) {
	if t.report.FirstDamaged == 0 {
		t.report.FirstDamaged, t.report.DamageReason = entry, reason
	}
}

// Report returns the counts of the read so far; they are complete once
// Read returned io.EOF.
func (t *TextReader) Report() Report { return t.report }

// Close releases the reader's buffers and its DATA cache. It does not
// close the file. Read fails after Close; Report still answers.
func (t *TextReader) Close() error {
	t.closed = true
	t.walk, t.buf = nil, nil
	return nil
}
