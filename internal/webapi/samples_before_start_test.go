package webapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// A line or an offset counted from the end that reaches before the
// start of the file is answered as missing (-1, null) under the key it
// was asked by, with status 200, as a position past the end is.
func TestSamples_PositionBeforeTheStartIsMissing(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	if err := os.WriteFile(f, []byte("A\nB\nC\nD\nE\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	ts := newTestServer(t)

	for _, tc := range []struct{ query, key string }{{"lines=-6", "-6"}, {"lines=-9--7", "-9--7"}, {"offsets=-99", "-99"}} {
		resp, err := http.Get(fmt.Sprintf("%s/v1/samples?path=%s&%s&context=0", ts.URL, f, tc.query))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.query, resp.StatusCode, body)
		}
		var out rxtypes.SamplesResponse
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		positions := out.Lines
		if tc.query[:7] == "offsets" {
			positions = out.Offsets
		}
		if v, ok := positions[tc.key]; !ok || v != -1 || len(positions) != 1 {
			t.Errorf("%s: positions = %v, want {%s: -1}", tc.query, positions, tc.key)
		}
		if s, ok := out.Samples[tc.key]; !ok || s != nil {
			t.Errorf("%s: samples = %s, want null under %q", tc.query, body, tc.key)
		}
	}
}
