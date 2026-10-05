package compression

import (
	"bytes"
	"testing"
)

// The journal signature is the longest one the probe reads: eight bytes
// name a systemd journal, and a file shorter than that is still named by
// the shorter signatures it starts with.
func TestDetectFromReader_NamesAJournalByItsEightByteSignature(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   Format
	}{
		{"journal header", append([]byte("LPKSHHRH"), 0, 0, 0, 0, 0x08, 0, 0, 0), FormatJournal},
		{"journal signature alone", []byte("LPKSHHRH"), FormatJournal},
		{"journal signature cut short", []byte("LPKSHHR"), FormatNone},
		{"text that starts like it", []byte("LPKSHHRX and then a log line\n"), FormatNone},
		{"gzip in two bytes", []byte{0x1f, 0x8b}, FormatGzip},
		{"bzip2 in three bytes", []byte("BZh"), FormatBz2},
		{"zstd in four bytes", []byte{0x28, 0xb5, 0x2f, 0xfd}, FormatZstd},
		{"xz in six bytes", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, FormatXz},
		{"seven bytes of text", []byte("LINE 1\n"), FormatNone},
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

// The name of a journal names no format: `rx compress system.journal`
// keeps the whole name and adds .zst.
func TestFormatFromExtension_AJournalNameNamesNoFormat(t *testing.T) {
	for _, name := range []string{"system.journal", "system@0123abcd-0000000000000001-00065db7fbffd8e8.journal", "user-1000.journal~"} {
		if got := FormatFromExtension(name); got != FormatNone {
			t.Errorf("FormatFromExtension(%q) = %q, want none", name, got)
		}
	}
}
