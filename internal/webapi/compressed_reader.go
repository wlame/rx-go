package webapi

import (
	"io"

	"github.com/wlame/rx-go/internal/compression"
)

// newCompressedReader is a thin wrapper around compression.NewReader.
// Post-Stage-8-consolidation (Finding 4) the webapi layer and the
// trace layer both route through internal/compression — no subprocess
// fork, pure-Go decoders, works on distroless containers.
//
// The webapi-local wrapper returns io.Reader (not io.ReadCloser) for
// backward compatibility with the existing handlers_samples call
// sites. Callers who want deterministic resource release must type-
// assert to io.Closer and Close() when done; for the tmp-file-backed
// flow in handlers_samples.go that's acceptable because the file's
// own Close handles the underlying bytes.
//
// R2M2 MIGRATION NOTE (2026-04-18): compression.NewReader now requires
// an io.ReadCloser source. This shim accepts the less-strict io.Reader
// and internally wraps with io.NopCloser. That preserves the pre-R2M2
// webapi contract — callers at handlers_samples.go pass *os.File
// values and keep their own defer file.Close(); they do NOT want the
// decompressor to close the file out from under them. If you change
// handlers_samples.go to let the wrapper own the file lifecycle, drop
// the NopCloser and pass the file directly (it already implements
// io.ReadCloser).
//
// Prefer compression.NewReader in new code — this shim exists only
// to minimize churn in existing call sites.
func newCompressedReader(src io.Reader, format compression.Format) (io.Reader, error) {
	r, err := compression.NewReader(io.NopCloser(src), format)
	if err != nil {
		return nil, err
	}
	// compression.NewReader returns an io.ReadCloser. Upcast to
	// io.Reader so the existing callers continue to compile; callers
	// that need Close can still assert.
	return r, nil
}
