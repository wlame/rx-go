package compression

import (
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// NewReader returns an io.ReadCloser that decompresses `src` according
// to `format`. Supported formats: gzip, bzip2, zstd (including
// seekable-zstd read linearly), and xz. FormatNone returns src wrapped
// as a pass-through closer.
//
// SINGLE-STATIC-BINARY MANDATE (decision 5.15, Stage 8 Finding 4):
//
// All decoders are pure Go — no subprocess fork for `gzip -d`,
// `xz -d`, `bzip2 -d`, or `zstd -d`. This keeps the rx binary usable
// on distroless / busybox containers that lack those external tools.
// Prior to Stage 8 the webapi layer used pure-Go readers but the
// trace engine shelled out; consolidating both through this helper
// enforces the mandate uniformly.
//
// PERFORMANCE NOTE: ulikunitz/xz is noticeably slower than the C
// xz-utils reference implementation for large files. In practice .xz
// files are rare in log workloads (gzip dominates) so the perf gap is
// acceptable at v1. If a user reports xz as a hot spot, we can add an
// opt-in subprocess path behind an env flag at that time.
//
// CLOSE SEMANTICS (R2M2 migration, 2026-04-18):
//
// The source MUST be an io.ReadCloser. The returned ReadCloser OWNS
// the source for lifetime purposes: calling Close on the returned
// wrapper also calls Close on src. Callers that want to retain
// ownership of the underlying reader should wrap with io.NopCloser
// explicitly:
//
//	r, err := compression.NewReader(io.NopCloser(src), format)
//
// This replaces the pre-migration contract where the wrapper's Close
// was a no-op for certain formats (bzip2, xz, FormatNone), forcing
// callers to defer a separate src.Close(). That pattern was
// error-prone and leaked file handles when the wrapper was the
// easier-to-see thing in the call site. The new contract closes both
// in a single deferred call, which is the Go-idiomatic composition
// pattern (like net/http.Response.Body wrapping the underlying
// connection).
//
// For formats where the decoder holds native resources (zstd's
// internal goroutines, xz's buffers), Close releases them BEFORE
// closing src. This ordering matters: the decoder may issue a final
// Read from src during its own Close to validate trailers; closing
// src first would race against that read.
func NewReader(src io.ReadCloser, format Format) (io.ReadCloser, error) {
	switch format {
	case FormatGzip:
		r, err := gzip.NewReader(src)
		if err != nil {
			// Caller retains src on init failure so THEY can close it.
			// Do NOT close src here — the caller's defer handles it.
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		return &chainCloser{wrapped: r, src: src}, nil
	case FormatBz2:
		// compress/bzip2.NewReader returns an io.Reader (no Close) and
		// doesn't report init errors — malformed data surfaces during
		// Read. Use closeFn to chain src-close into the wrapper.
		return &chainCloser{wrapped: io.NopCloser(bzip2.NewReader(src)), src: src}, nil
	case FormatZstd, FormatSeekableZstd:
		// klauspost/compress's zstd.NewReader decodes both standard
		// zstd streams and seekable-zstd files — the "skippable frames"
		// that carry the seek index are ignored on a linear read.
		// Seekable-zstd is the same frame format; the decoder doesn't
		// know or care about the index.
		//
		// POOLING NOTE: the decoder pool in decoder_pool.go does NOT
		// apply to this call site. Pool reuse works for stateless
		// DecodeAll calls on a single pre-buffered frame; the streaming
		// zstd.NewReader(src) wrapper holds per-instance state (window
		// buffers and internal goroutines tied to this specific source),
		// so it must be constructed fresh per stream and closed when
		// done. The per-frame path in internal/seekable/decoder.go uses
		// the pool instead.
		r, err := zstd.NewReader(src)
		if err != nil {
			return nil, fmt.Errorf("zstd reader: %w", err)
		}
		return &chainCloser{wrapped: zstdReaderCloser{r: r}, src: src}, nil
	case FormatXz:
		r, err := xz.NewReader(src)
		if err != nil {
			return nil, fmt.Errorf("xz reader: %w", err)
		}
		return &chainCloser{wrapped: io.NopCloser(r), src: src}, nil
	case FormatNone:
		// Uncompressed — pass through without decompression, but still
		// chain-close so src is released on wrapper.Close.
		return &chainCloser{wrapped: io.NopCloser(src), src: src}, nil
	default:
		return nil, fmt.Errorf("unsupported compression format: %q", format)
	}
}

// chainCloser composes a decoder wrapper with the underlying source
// into a single io.ReadCloser whose Close releases BOTH. The Read
// method delegates to the decoder; Close calls the decoder's Close
// first (to flush / validate trailing state), then src.Close().
//
// The two close calls are independent: a failure on the decoder does
// not prevent src from being closed. Errors are joined with
// errors.Join so the caller sees both.
type chainCloser struct {
	wrapped io.ReadCloser
	src     io.Closer
}

// Read forwards into the decoder.
func (c *chainCloser) Read(p []byte) (int, error) { return c.wrapped.Read(p) }

// Close closes the decoder wrapper then the source, returning the
// joined error (or nil if both succeeded). Order matters: the
// decoder's Close may read residual bytes from src (e.g. gzip CRC
// validation), so src must stay open through that call.
func (c *chainCloser) Close() error {
	wErr := c.wrapped.Close()
	sErr := c.src.Close()
	if wErr == nil {
		return sErr
	}
	if sErr == nil {
		return wErr
	}
	return errors.Join(wErr, sErr)
}

// zstdReaderCloser adapts *zstd.Decoder's Close() signature (which
// returns no error) to io.Closer.
type zstdReaderCloser struct {
	r *zstd.Decoder
}

func (z zstdReaderCloser) Read(p []byte) (int, error) { return z.r.Read(p) }

// Close releases the decoder's internal buffers.
func (z zstdReaderCloser) Close() error {
	z.r.Close()
	return nil
}
