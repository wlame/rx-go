package index

import (
	"io"
	"os"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
	"github.com/stretchr/testify/require"
)

// newGzipWriter wraps klauspost's gzip writer for test fixture creation.
func newGzipWriter(w io.Writer) (*kgzip.Writer, error) {
	return kgzip.NewWriterLevel(w, kgzip.DefaultCompression)
}

// createGzipTestFile creates a gzip-compressed file with the given content.
func createGzipTestFile(t *testing.T, path string, content []byte) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	gw, err := newGzipWriter(f)
	require.NoError(t, err)

	_, err = gw.Write(content)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
}
