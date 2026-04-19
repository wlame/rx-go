package compression

import (
	"sync"

	"github.com/klauspost/compress/zstd"
)

// decoderPool reuses zstd.Decoder instances. Each decoder is created once with
// zstd.NewReader(nil) (stateless single-buffer mode) which allocates internal
// decoding tables (~2 MB). Reusing one decoder for many frames avoids the
// table allocation on each call — a ~5x speedup on cold-index builds of
// seekable-zstd files with many frames.
//
// Usage pattern:
//
//	d := AcquireDecoder()
//	defer ReleaseDecoder(d)
//	out, err := d.DecodeAll(compressedFrame, nil)
//
// The decoder is stateless across DecodeAll calls — no Reset required.
// Put is safe to call from any goroutine.
//
// Go note: sync.Pool is the standard library's primitive for amortizing
// expensive allocations across goroutines. The pool's New func is called
// when Get finds the pool empty; items returned via Put may be GC'd at
// any time, so the pool is a best-effort cache, not a guarantee of reuse.
// For hot paths with many frames this is exactly what we want.
var decoderPool = sync.Pool{
	New: func() any {
		// zstd.NewReader(nil) never errors for nil source — the returned
		// decoder is used only via DecodeAll, which is stateless.
		d, _ := zstd.NewReader(nil)
		return d
	},
}

// AcquireDecoder returns a pooled zstd.Decoder suitable for stateless
// DecodeAll calls. Every Acquire must be paired with a ReleaseDecoder.
func AcquireDecoder() *zstd.Decoder {
	return decoderPool.Get().(*zstd.Decoder)
}

// ReleaseDecoder returns a decoder to the pool for reuse. The decoder
// retains its internal tables (~2 MB) — this is the whole point of pooling.
// Safe to call with a nil decoder (no-op).
func ReleaseDecoder(d *zstd.Decoder) {
	if d == nil {
		return
	}
	decoderPool.Put(d)
}
