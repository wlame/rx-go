package sigscan

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// A stream-compressed file (gzip, bzip2, xz, zstd without a seek table)
// can only be decoded from its first byte to its last, by one
// goroutine. That goroutine (the decoder) cuts the text into batches of
// whole lines and hands them to the workers; the merge takes the
// results back in batch order. A batch's buffer is held until the merge
// is done with it, because a block that crosses into the batch is
// finished by walking the batch's lines again.

// batch is a run of whole lines of a decoded stream.
type batch struct {
	seq int
	// off is the text offset of data's first byte.
	off int64
	// data holds whole lines, each with its '\n', except that the last
	// line of the text may lack one (final batch only).
	data []byte
	// long, when set, replaces data: the batch is one line longer than
	// a buffer, kept as its first bytes.
	long *longLine
	// final says no text follows the batch.
	final bool
	// buf is the pool buffer the batch holds; it goes back to the pool
	// after the merge.
	buf []byte
}

// longLine is a line longer than a batch buffer: its first bytes
// (enough for its view), its full length without the "\n", its last
// byte (for "\r\n") and whether it ends with a "\n".
type longLine struct {
	prefix []byte
	length int64
	last   byte
	ended  bool
}

// walkBatch feeds a batch's lines to fn.
func walkBatch(b *batch, lineBytes int, fn lineFunc) {
	sp := newSplitter(lineBytes, b.off, noLimit, fn)
	if b.long != nil {
		sp.emit(b.long.prefix, b.long.length, b.long.last, b.off, b.long.ended)
		return
	}
	if sp.push(b.data) && b.final {
		sp.finish()
	}
}

// decoder reads a decompressed stream and cuts it into batches.
type decoder struct {
	r          io.Reader
	lineBytes  int
	batchBytes int
	// batchLines > 0 cuts a batch after that many lines (tests).
	batchLines int
	// free holds buffers the merge gave back; made counts the buffers
	// made so far, at most maxBufs. A batch holds one buffer, so the
	// batches decoded but not yet merged are at most maxBufs.
	free    chan []byte
	made    int
	maxBufs int
	// carry is the start of a line that did not fit before the cut.
	carry []byte
	off   int64
	seq   int
	// progress reports the compressed bytes read (through the counting
	// reader under r).
	onRead func()
}

// acquire returns a buffer for the next batch: a free one, a new one
// while fewer than maxBufs exist, or the next one the merge gives back.
//
// Go note: select waits on whichever channel is ready first; the
// ctx.Done() case ends the wait when the scan is canceled.
func (d *decoder) acquire(ctx context.Context) ([]byte, error) {
	select {
	case buf := <-d.free:
		return buf, nil
	default:
	}
	if d.made < d.maxBufs {
		d.made++
		return make([]byte, d.batchBytes), nil
	}
	select {
	case buf := <-d.free:
		return buf, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run decodes the whole stream and sends its batches to out in order.
// It returns the number of batches sent and the decoding error, if any.
func (d *decoder) run(ctx context.Context, out chan<- *batch) (int, error) {
	for {
		buf, err := d.acquire(ctx)
		if err != nil {
			return d.seq, err
		}
		b, err := d.nextBatch(buf)
		if err != nil {
			return d.seq, err
		}
		b.seq = d.seq
		d.seq++
		select {
		case out <- b:
		case <-ctx.Done():
			return d.seq, ctx.Err()
		}
		if b.final {
			return d.seq, nil
		}
	}
}

// nextBatch fills buf with the carried bytes and more text, and cuts
// the batch: after the last whole line (or after batchLines lines), or,
// when buf holds no line break at all, as one long line read to its
// end.
func (d *decoder) nextBatch(buf []byte) (*batch, error) {
	n := copy(buf, d.carry)
	d.carry = d.carry[:0]
	n, eof, err := d.fill(buf, n)
	if err != nil {
		return nil, err
	}
	data := buf[:n]
	b := &batch{off: d.off, buf: buf}
	if eof {
		b.data, b.final = data, true
		d.off += int64(n)
		return b, nil
	}
	if cut := d.cutPoint(data); cut > 0 {
		d.carry = append(d.carry, data[cut:]...)
		b.data = data[:cut]
		d.off += int64(cut)
		return b, nil
	}
	return d.longBatch(b, data)
}

// fill reads into buf[n:] until buf is full or the stream ends.
func (d *decoder) fill(buf []byte, n int) (int, bool, error) {
	for n < len(buf) {
		m, err := d.r.Read(buf[n:])
		n += m
		if d.onRead != nil {
			d.onRead()
		}
		if errors.Is(err, io.EOF) {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
	}
	return n, false, nil
}

// cutPoint returns where the batch in data ends: after its last line
// break, or after batchLines lines; 0 when data holds no line break.
func (d *decoder) cutPoint(data []byte) int {
	if d.batchLines <= 0 {
		return bytes.LastIndexByte(data, '\n') + 1
	}
	pos := 0
	for i := 0; i < d.batchLines; i++ {
		j := bytes.IndexByte(data[pos:], '\n')
		if j < 0 {
			break
		}
		pos += j + 1
	}
	return pos
}

// longBatch reads the rest of a line that fills a whole buffer (data,
// with no line break), keeping only its first bytes, and returns it as
// a batch of its own. The bytes after its line break are carried into
// the next batch. buf is reused for the reads, so the line costs at
// most its kept bytes and one buffer.
func (d *decoder) longBatch(b *batch, data []byte) (*batch, error) {
	keep := d.lineBytes + len(utf8BOM)
	ll := &longLine{prefix: bytes.Clone(data[:min(len(data), keep)]), length: int64(len(data)), last: lastByte(data)}
	buf := b.buf
	for {
		n, eof, err := d.fill(buf, 0)
		if err != nil {
			return nil, err
		}
		piece := buf[:n]
		if i := bytes.IndexByte(piece, '\n'); i >= 0 {
			ll.addBytes(piece[:i], keep)
			ll.ended = true
			d.carry = append(d.carry, piece[i+1:]...)
			b.long = ll
			d.off += ll.length + 1
			// A long line that ends the text with its '\n' leaves
			// nothing to carry; the next batch is then the final one.
			return b, nil
		}
		ll.addBytes(piece, keep)
		if eof {
			b.long, b.final = ll, true
			d.off += ll.length
			return b, nil
		}
	}
}

// addBytes adds the next bytes of a long line.
func (ll *longLine) addBytes(p []byte, keep int) {
	if len(p) == 0 {
		return
	}
	if room := keep - len(ll.prefix); room > 0 {
		ll.prefix = append(ll.prefix, p[:min(room, len(p))]...)
	}
	ll.length += int64(len(p))
	ll.last = p[len(p)-1]
}
