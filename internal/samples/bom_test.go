package samples

import (
	"testing"
)

// A file whose first line starts with a UTF-8 byte-order mark gives
// line 1's timestamp everywhere: the time range's first time, with an
// index and without, and the line_timestamps of a sample of line 1, on
// the plain, the gzip and the seekable copy alike.
func TestTimeRange_LineOneBehindAByteOrderMark(t *testing.T) {
	t.Setenv("RX_LOG_TZ", "")
	lines := isoLines(orderedValues(50))
	firstMs := lines[0].ms
	copies := timeCopies(t, append([]byte("\xEF\xBB\xBF"), textOf(lines)...))

	for name, path := range copies {
		t.Run(name, func(t *testing.T) {
			for _, loader := range []IndexLoader{NoIndex, indexedLoader(t, path)} {
				r := timeRangeOf(t, path, loader)
				if r.FirstMs != nil && *r.FirstMs != firstMs {
					t.Errorf("first_ms %d, want line 1's %d (%s)", *r.FirstMs, firstMs, rangeText(r))
				}
				if r.FirstMs == nil && name != "gzip" {
					t.Errorf("first_ms null, want line 1's %d (%s)", firstMs, rangeText(r))
				}
				resp, err := Resolve(t.Context(), Request{Path: path, IndexLoader: loader, Lines: []OffsetOrRange{{Start: 1}}})
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if got := resp.LineTimestamps["1"]; len(got) != 1 || got[0] == nil || *got[0] != firstMs {
					t.Errorf("line_timestamps[1] = %v, want line 1's %d", got, firstMs)
				}
			}
		})
	}
}
