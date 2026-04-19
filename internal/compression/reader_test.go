package compression

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// TestNewReader_GzipRoundTrip verifies gzip decode parity. We produce
// a gzip stream from a known plaintext, feed it to NewReader, and
// assert the output equals the plaintext.
func TestNewReader_GzipRoundTrip(t *testing.T) {
	plaintext := []byte("hello gzip world\nanother line\n")

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(plaintext)
	_ = gw.Close()

	r, err := NewReader(io.NopCloser(&buf), FormatGzip)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip mismatch:\n got:  %q\n want: %q", got, plaintext)
	}
}

// TestNewReader_XzRoundTrip is the critical static-binary test — proves
// that `rx trace foo.xz` works without an `xz` binary on PATH. This
// was the motivating example in Stage 8 Finding 4.
func TestNewReader_XzRoundTrip(t *testing.T) {
	plaintext := []byte("hello xz world\nsome payload\n")

	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz.NewWriter: %v", err)
	}
	if _, err := xw.Write(plaintext); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = xw.Close()

	r, err := NewReader(io.NopCloser(&buf), FormatXz)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip mismatch:\n got:  %q\n want: %q", got, plaintext)
	}
}

// TestNewReader_ZstdRoundTrip — same for zstd.
func TestNewReader_ZstdRoundTrip(t *testing.T) {
	plaintext := []byte("hello zstd world\nzstd payload line\n")

	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := zw.Write(plaintext); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = zw.Close()

	r, err := NewReader(io.NopCloser(&buf), FormatZstd)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip mismatch:\n got:  %q\n want: %q", got, plaintext)
	}
}

// TestNewReader_NoneReturnsRawData — FormatNone is the pass-through
// case. Useful for call sites that don't want to branch on format
// before piping into rg.
func TestNewReader_NoneReturnsRawData(t *testing.T) {
	plaintext := []byte("not compressed at all\n")
	r, err := NewReader(io.NopCloser(bytes.NewReader(plaintext)), FormatNone)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("pass-through mismatch:\n got:  %q\n want: %q", got, plaintext)
	}
}

// TestNewReader_UnsupportedFormat returns a non-nil error.
func TestNewReader_UnsupportedFormat(t *testing.T) {
	_, err := NewReader(io.NopCloser(bytes.NewReader([]byte("x"))), Format("made-up-format"))
	if err == nil {
		t.Error("expected error for unsupported format")
	}
}

// spyReadCloser is a bytes.Reader-backed io.ReadCloser that records
// whether Close() has been called. Used to verify the R2M2 contract:
// closing the wrapper returned by NewReader also closes the source.
type spyReadCloser struct {
	r      *bytes.Reader
	closed bool
}

func newSpyReadCloser(data []byte) *spyReadCloser {
	return &spyReadCloser{r: bytes.NewReader(data)}
}

func (s *spyReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *spyReadCloser) Close() error {
	s.closed = true
	return nil
}

// TestNewReader_CloseClosesSource_Gzip is the R2M2 regression guard:
// the ReadCloser returned by NewReader must Close the source when
// the caller closes the wrapper. Before the migration the noopCloser
// shim for FormatBz2/Xz/None was a no-op, leaking file handles
// whenever the caller only held the wrapper.
//
// Table-driven across all formats that previously used the no-op
// closer or benefit from source-close composition. Each case builds
// a valid compressed stream, wraps the compressed bytes in a spy,
// calls NewReader(spy, format), then asserts that r.Close() ALSO
// closes the spy.
func TestNewReader_CloseClosesSource_AllFormats(t *testing.T) {
	plaintext := []byte("payload for r2m2 close-chain test\n")

	buildGzip := func() []byte {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write(plaintext)
		_ = gw.Close()
		return buf.Bytes()
	}
	buildXz := func() []byte {
		var buf bytes.Buffer
		xw, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatalf("xz.NewWriter: %v", err)
		}
		_, _ = xw.Write(plaintext)
		_ = xw.Close()
		return buf.Bytes()
	}
	buildZstd := func() []byte {
		var buf bytes.Buffer
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
		}
		_, _ = zw.Write(plaintext)
		_ = zw.Close()
		return buf.Bytes()
	}

	cases := []struct {
		name    string
		format  Format
		payload []byte
	}{
		{"gzip", FormatGzip, buildGzip()},
		{"xz_previously_noopCloser", FormatXz, buildXz()},
		{"zstd", FormatZstd, buildZstd()},
		{"seekable_zstd", FormatSeekableZstd, buildZstd()},
		{"none_previously_noopCloser", FormatNone, plaintext},
		// bzip2 is harder to synthesize from stdlib (compress/bzip2
		// is decode-only). Covered indirectly by the format=Bz2
		// closure test below with a pre-built minimal stream.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newSpyReadCloser(tc.payload)
			r, err := NewReader(spy, tc.format)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if _, err := io.ReadAll(r); err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if spy.closed {
				t.Fatalf("spy.Close called before wrapper.Close — premature close")
			}
			if err := r.Close(); err != nil {
				t.Fatalf("wrapper Close: %v", err)
			}
			if !spy.closed {
				t.Errorf("wrapper.Close did not close source spy — R2M2 contract broken")
			}
		})
	}
}

// TestNewReader_CloseClosesSource_Bzip2 covers the bzip2 format
// separately. We pre-build a minimal bzip2 stream via the external
// stdlib's compress/bzip2 which is decode-only — but we can round-trip
// with a tiny canned stream produced out-of-band. For the purposes
// of this close-propagation test we only need enough bytes for
// NewReader to accept the stream; we don't assert on the decoded
// content.
func TestNewReader_CloseClosesSource_Bzip2(t *testing.T) {
	// Minimal valid bzip2 stream for the string "a\n". Generated
	// once with `bzip2 -c` and pasted here. If upstream bzip2 spec
	// ever changes we regenerate; this is test-fixture data.
	// 00 "BZh9" header + block content + block-end marker.
	canned := []byte{
		0x42, 0x5a, 0x68, 0x39, 0x31, 0x41, 0x59, 0x26, 0x53, 0x59,
		0xc3, 0xba, 0x9a, 0x46, 0x00, 0x00, 0x00, 0x40, 0x00, 0x40,
		0x00, 0x20, 0x00, 0x21, 0x00, 0x82, 0x0b, 0xb9, 0x22, 0x9c,
		0x28, 0x48, 0x61, 0xdd, 0x4d, 0x23, 0x00,
	}
	spy := newSpyReadCloser(canned)
	r, err := NewReader(spy, FormatBz2)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// Read to EOF; bzip2 decoder may surface errors on malformed
	// content but for our canned stream it should be fine or a
	// trailing error. Either way we then call Close and assert.
	_, _ = io.ReadAll(r)
	if spy.closed {
		t.Fatalf("spy.Close called before wrapper.Close")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("wrapper Close: %v", err)
	}
	if !spy.closed {
		t.Errorf("wrapper.Close did not close source spy — R2M2 contract broken")
	}
}
