package webapi

import "testing"

// /health gives the viewer the interval, in seconds, by which it reads
// the loaded folders of its files list again: the server's own setting
// when `rx serve` passes one (0 included, which turns the refresh off),
// else RX_TREE_REFRESH_SECONDS, else 5.
func TestHealth_TreeRefreshSecondsIsTheServersSetting(t *testing.T) {
	off, every30 := 0, 30
	cases := []struct {
		name string
		env  string
		set  *int
		want int
	}{
		{"default", "", nil, 5},
		{"the variable", "12", nil, 12},
		{"the server's setting over the variable", "12", &every30, 30},
		{"the server's setting turns it off", "12", &off, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_TREE_REFRESH_SECONDS", tc.env)
			got := buildHealthResponse(NewServer(Config{TreeRefreshSeconds: tc.set})).TreeRefreshSeconds
			if got != tc.want {
				t.Errorf("tree_refresh_seconds %d, want %d", got, tc.want)
			}
		})
	}
}
