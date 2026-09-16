package compressfile

import "testing"

// The default output takes the input's name with its compression suffix
// replaced by .zst, since the output holds the text and not the
// compressed bytes; any other name gets .zst appended.
func TestDefaultOutputName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/logs/app.log.gz", "app.log.zst"},
		{"/logs/app.log.gzip", "app.log.zst"},
		{"/logs/app.log.bz2", "app.log.zst"},
		{"/logs/app.log.bzip2", "app.log.zst"},
		{"/logs/app.log.xz", "app.log.zst"},
		{"/logs/app.log.zst", "app.log.zst"},
		{"/logs/app.log.zstd", "app.log.zst"},
		{"/logs/APP.LOG.GZ", "APP.LOG.zst"},
		{"/logs/app.log", "app.log.zst"},
		{"/logs/app", "app.zst"},
		{"/logs/app.gz.log", "app.gz.log.zst"},
		{"app.log.gz", "app.log.zst"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := DefaultOutputName(tc.input); got != tc.want {
				t.Errorf("DefaultOutputName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
