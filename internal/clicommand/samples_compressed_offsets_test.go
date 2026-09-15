package clicommand

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// `rx samples --offsets=B file.gz` answers with the line holding
// decompressed offset B, and `--lines=N` reports the offset that leads
// back to N. The first run builds the file's index and the second reads
// it; the answers are the same.
func TestSamples_OffsetsAndLinesAreInversesOnAGzipFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RX_CACHE_DIR", filepath.Join(dir, "cache"))

	var text bytes.Buffer
	for n := 1; n <= 300; n++ {
		fmt.Fprintf(&text, "LINE %d of the log\n", n)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(text.Bytes())
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	path := filepath.Join(dir, "app.log.gz")
	if err := os.WriteFile(path, gz.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	run := func(params samplesParams) rxtypes.SamplesResponse {
		t.Helper()
		params.path, params.jsonOutput = path, true
		var out bytes.Buffer
		if err := runSamples(&out, params); err != nil {
			t.Fatalf("runSamples(%+v): %v", params, err)
		}
		var resp rxtypes.SamplesResponse
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return resp
	}

	for _, pass := range []string{"cache empty", "index built"} {
		byLine := run(samplesParams{lines: []string{"217"}})
		offset := byLine.Lines["217"]
		if offset <= 0 {
			t.Fatalf("%s: --lines=217 reports offset %d", pass, offset)
		}
		byOffset := run(samplesParams{offsets: []string{strconv.FormatInt(offset, 10)}})
		key := strconv.FormatInt(offset, 10)
		if byOffset.Offsets[key] != 217 {
			t.Fatalf("%s: --offsets=%d reports line %d, want 217", pass, offset, byOffset.Offsets[key])
		}
		if got := byOffset.Samples[key]; len(got) != 1 || got[0] != "LINE 217 of the log" {
			t.Fatalf("%s: --offsets=%d sample %q", pass, offset, got)
		}
	}
}
