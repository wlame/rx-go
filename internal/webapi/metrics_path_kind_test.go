package webapi

import "testing"

// A request is labeled by the costliest kind of file it names: a
// journal, read from its start through a decoder, is a compressed file.
func TestPathKindOf_LabelsByTheFileKind(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{[]string{"/var/log/app.log"}, "regular"},
		{[]string{"/var/log/app.log.gz"}, "compressed"},
		{[]string{"/var/log/app.log.zst"}, "seekable"},
		{[]string{"/var/log/journal/0123abcd/system.journal"}, "compressed"},
		{[]string{"/var/log/app.log", "/var/log/journal/0123abcd/SYSTEM.JOURNAL"}, "compressed"},
	}
	for _, tc := range cases {
		if got := pathKindOf(tc.paths); got != tc.want {
			t.Errorf("pathKindOf(%q) = %q, want %q", tc.paths, got, tc.want)
		}
	}
}
