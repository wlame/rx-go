package samples

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/filekind"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/timestamps"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// storedValues are the stamps a scan or an index keeps for every line
// of the file at path that has a timestamp of its own: the value in the
// file's own frame and the zone offset the line writes, by line number.
func storedValues(t *testing.T, path string) map[int64]timestamps.Stamp {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	format, ok, err := index.DetectTimeFormat(f, info.Size(), filekind.Kind{})
	if err != nil || !ok {
		t.Fatalf("detect: %v %v", ok, err)
	}
	parser, err := timestamps.NewParser(format, info.ModTime().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values := map[int64]timestamps.Stamp{}
	line := int64(1)
	for start := 0; start < len(data); line++ {
		end := start
		for end < len(data) && data[end] != '\n' {
			end++
		}
		if s, ok := index.LineStamp(parser, data[start:end]); ok {
			values[line] = s
		}
		start = end + 1
	}
	return values
}

// The instant StoredTimes gives for a line's stored value and the offset
// the line writes is the line_timestamps value samples gives the line
// under the same file zone: for a file whose timestamps carry no zone,
// one whose offset changes (and with a line that writes none), and one
// of epoch values; with a line index and without one, which never
// changes the answer.
func TestStoredTimes_GiveTheInstantsOfLineTimestamps(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	t.Setenv("RX_LOG_TZ", "Europe/Berlin")
	dir := t.TempDir()
	files := map[string]string{
		"local.log": "2026-03-29 01:59:58 INFO LINE 1\nno stamp LINE 2\n2026-03-29 03:00:01 INFO LINE 3\n2026-03-29 03:00:02 INFO LINE 4\n",
		"zoned.log": "2026-03-29T01:59:58+01:00 INFO LINE 1\nno stamp LINE 2\n2026-03-29T03:00:01+02:00 INFO LINE 3\n" +
			"2026-03-29T03:00:02 INFO LINE 4\n",
		"epoch.log": "1774745998 INFO LINE 1\nno stamp LINE 2\n1774746001 INFO LINE 3\n1774746002.500 INFO LINE 4\n",
	}
	lastLine := int64(4)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		idx, err := index.Build(path, index.BuildOptions{StepBytes: 16})
		if err != nil {
			t.Fatal(err)
		}
		stored := storedValues(t, path)
		if len(stored) != 3 {
			t.Fatalf("%s: %d stamped lines, want 3", name, len(stored))
		}
		loaders := map[string]IndexLoader{
			"no index": NoIndex,
			"index":    func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil },
		}
		for _, zoneName := range []string{"", "Asia/Tokyo", "+05:30", "UTC"} {
			fileZone := config.Zone{}
			if zoneName != "" {
				if fileZone, err = config.ParseZone(zoneName); err != nil {
					t.Fatal(err)
				}
			}
			for loaderName, loader := range loaders {
				label := name + "/" + zoneName + "/" + loaderName
				src, err := paths.Pin(path)
				if err != nil {
					t.Fatal(err)
				}
				times, err := StoredTimesOf(StoredTimesRequest{
					Path: path, Source: src, IndexLoader: loader, FileZone: fileZone.Location, LogZone: berlin,
				})
				if err != nil || times == nil {
					t.Fatalf("%s: StoredTimesOf = %v, %v", label, times, err)
				}
				oracle, err := Resolve(context.Background(), Request{
					Path: path, Source: src, Lines: []OffsetOrRange{{Start: 1, End: &lastLine}}, FileZone: fileZone, IndexLoader: loader,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := oracle.LineTimestamps["1-4"]
				for line, stamp := range stored {
					got, ok := times.Instant(stamp.Ms, stamp.OffsetMinutes)
					if !ok || want[line-1] == nil || got != *want[line-1] {
						t.Errorf("%s line %d: %d %v, want line_timestamps %v", label, line, got, ok, deref(want[line-1]))
					}
				}
			}
		}
	}
}

// A file without a timestamp format has no StoredTimes.
func TestStoredTimes_FileWithoutTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.log")
	if err := os.WriteFile(path, []byte("no time here\nnor here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	times, err := StoredTimesOf(StoredTimesRequest{Path: path, Source: src, IndexLoader: NoIndex})
	if err != nil || times != nil {
		t.Fatalf("StoredTimesOf = %v, %v; want nil, nil", times, err)
	}
}

// deref renders a nullable value for a message.
func deref(p *int64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatInt(*p, 10)
}
