package trace

import (
	"bytes"
	"compress/gzip"
)

// gzipBytesImpl is the implementation used by coverage_extra_test.go.
// Split into its own file so the stdlib import doesn't pollute other
// test files.
func gzipBytesImpl(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(src); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
