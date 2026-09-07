package index

import (
	"bytes"
	"io"
	"os"

	"github.com/wlame/rx-go/internal/compression"
)

// textSampleSize is how many bytes are read from the head of a file to
// decide whether it is text. Matches rx-python's is_text_file.
const textSampleSize = 8192

// IsTextFile reports whether a file looks like text rather than a binary
// blob, using the same rule as rx-python: a NUL byte anywhere in the
// first 8 KiB means binary.
//
// A compressed file is text by definition here — the check is about what
// the file holds once decompressed, and the compressed bytes themselves
// are full of NULs. Callers index compressed files through the
// decompressor, so the sample would be meaningless.
//
// An unreadable file reports false, which lands it in the skipped list
// rather than the error list, again matching rx-python.
func IsTextFile(path string) bool {
	if format, _ := compression.DetectFromPath(path); format != compression.FormatNone {
		return true
	}
	f, err := os.Open(path) //nolint:gosec // path comes from a validated search root
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	sample := make([]byte, textSampleSize)
	n, err := f.Read(sample)
	if err != nil && err != io.EOF {
		return false
	}
	return !bytes.Contains(sample[:n], []byte{0})
}
