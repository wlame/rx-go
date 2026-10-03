package compression

import (
	"bytes"
	"testing"
)

func TestFormatFromExtension(t *testing.T) {
	cases := []struct {
		path string
		want Format
	}{
		{"foo.gz", FormatGzip},
		{"foo.GZ", FormatGzip},
		{"foo.gzip", FormatGzip},
		{"foo.zst", FormatZstd},
		{"foo.zstd", FormatZstd},
		{"foo.xz", FormatXz},
		{"foo.bz2", FormatBz2},
		{"foo.bzip2", FormatBz2},
		{"foo.log", FormatNone},
		{"foo", FormatNone},
		{"foo.tar.gz", FormatGzip},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := FormatFromExtension(tc.path); got != tc.want {
				t.Errorf("FormatFromExtension(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestDetectFromReader_MagicBytes(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   Format
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, FormatGzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}, FormatZstd},
		{"zstd skippable frame 0x184D2A50", []byte{0x50, 0x2a, 0x4d, 0x18, 4, 0}, FormatZstd},
		{"zstd skippable frame 0x184D2A5E", []byte{0x5e, 0x2a, 0x4d, 0x18, 4, 0}, FormatZstd},
		{"not a skippable frame", []byte{0x60, 0x2a, 0x4d, 0x18, 4, 0}, FormatNone},
		{"xz", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, FormatXz},
		{"bz2", []byte{0x42, 0x5a, 0x68, 0x39}, FormatBz2},
		{"text", []byte("LINE 1 plain\n"), FormatNone},
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03}, FormatNone},
		{"empty", []byte{}, FormatNone},
		{"short", []byte{0x1f}, FormatNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectFromReader(bytes.NewReader(tc.header))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
