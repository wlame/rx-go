package webapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A frame_size below one byte is out of range (422, as a compression
// level outside 1..22 is); one that cannot be read at all is malformed
// (400, as before). A valid size is accepted.
func TestCompress_FrameSizeBelowOneByteIs422(t *testing.T) {
	// One input per case: a compress task holds its input while it runs,
	// so a second request for the same file would answer 409.
	files := sandboxedFiles(t, map[string]string{
		"zero.log": "alpha\n", "fraction.log": "alpha\n", "negative.log": "alpha\n",
		"words.log": "alpha\n", "valid.log": "alpha\n",
	})
	ts := newTestServer(t)

	for _, tc := range []struct {
		input      string
		frameSize  string
		wantStatus int
		wantDetail string
	}{
		{"zero.log", "0", http.StatusUnprocessableEntity, "frame_size must be at least 1 byte"},
		{"fraction.log", "0.1B", http.StatusUnprocessableEntity, "frame_size must be at least 1 byte"},
		{"negative.log", "-1", http.StatusUnprocessableEntity, "frame_size must be at least 1 byte"},
		{"words.log", "lots", http.StatusBadRequest, "frame_size \"lots\""},
		{"valid.log", "4M", http.StatusOK, ""},
	} {
		body := fmt.Sprintf(`{"input_path": %q, "frame_size": %q, "build_index": false}`, files[tc.input], tc.frameSize)
		status, answer := postCompressRaw(t, ts.URL, body)
		if status != tc.wantStatus {
			t.Errorf("frame_size %q: status %d, want %d (%v)", tc.frameSize, status, tc.wantStatus, answer)
		}
		detail, _ := answer["detail"].(string)
		if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
			t.Errorf("frame_size %q: detail %q, want %q", tc.frameSize, detail, tc.wantDetail)
		}
	}
}
