package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureIndexLog sends slog's default logger to a buffer for the rest
// of the test.
func captureIndexLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// storedIndexFixture writes a small log, builds and stores its index,
// and returns the log's path and the index file's path.
func storedIndexFixture(t *testing.T) (source, cachePath string) {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	source = filepath.Join(t.TempDir(), "app.log")
	writePlain(t, source, numberedText(200, "LINE"))
	built, err := Build(source, BuildOptions{StepBytes: 512})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cachePath, err = Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return source, cachePath
}

// rewriteTimeIndex gives the stored index at cachePath a time section,
// changed by edit, which also learns how many checkpoints the index
// has. The rest of the index stays valid.
func rewriteTimeIndex(t *testing.T, cachePath string, edit func(ti map[string]any, checkpoints int)) {
	t.Helper()
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	checkpoints := len(doc["line_index"].([]any))
	ti := map[string]any{
		"format": "iso", "anchored": true, "day_first": nil, "has_zone": false,
		"year_from_mtime": false, "timestamped_lines": 0, "first": nil, "last": nil,
		"first_zone_offset_minutes": nil, "backward_steps": 0, "max_backward_ms": 0,
		"max_before": make([]any, checkpoints),
	}
	edit(ti, checkpoints)
	doc["time_index"] = ti
	if body, err = json.Marshal(doc); err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// An index file that cannot be read or parsed — cut short by a power
// loss or a full disk, or left unreadable by its permissions — is not
// an index. It is reported as absent, the way a missing one is, so a
// lookup answers without it and a build replaces it; one warning names
// the file so the operator hears about it.
func TestLoadFromPath_DamagedIndexIsAbsentAndLogged(t *testing.T) {
	damages := []struct {
		name   string
		damage func(t *testing.T, cachePath string)
	}{
		{"truncated", func(t *testing.T, cachePath string) {
			body, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatalf("read index: %v", err)
			}
			if err := os.WriteFile(cachePath, body[:len(body)/2], 0o600); err != nil {
				t.Fatalf("truncate index: %v", err)
			}
		}},
		{"unreadable", func(t *testing.T, cachePath string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its permissions")
			}
			if err := os.Chmod(cachePath, 0); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(cachePath, 0o600) })
		}},
		// A time section a search cannot trust: max_before must have one
		// entry per checkpoint, never decrease, and name a known format.
		{"time section misaligned with the checkpoints", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints int) {
				ti["max_before"] = make([]any, checkpoints+1)
			})
		}},
		{"time section whose maximum decreases", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints int) {
				values := make([]any, checkpoints)
				for i := range values {
					values[i] = 1_000_000 - i
				}
				ti["max_before"] = values
			})
		}},
		{"time section of an unknown format", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints int) {
				ti["format"] = "stardate"
				ti["max_before"] = make([]any, checkpoints)
			})
		}},
		// The fixture has 200 lines; first, last and the count of
		// timestamped lines must describe lines it has.
		{"time section with a negative count of timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
				ti["timestamped_lines"] = -1
			})
		}},
		{"time section without first and last for its timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
				ti["timestamped_lines"] = 3
			})
		}},
		{"time section whose first line is line 0", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
				ti["timestamped_lines"] = 3
				ti["first"] = map[string]any{"ms": 1, "line": 0, "offset": 0}
				ti["last"] = map[string]any{"ms": 2, "line": 200, "offset": 0}
			})
		}},
		{"time section whose last line is past the last line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
				ti["timestamped_lines"] = 3
				ti["first"] = map[string]any{"ms": 1, "line": 1, "offset": 0}
				ti["last"] = map[string]any{"ms": 2, "line": 201, "offset": 0}
			})
		}},
		{"time section with a zone offset beyond 18 hours", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
				ti["has_zone"] = true
				ti["first_zone_offset_minutes"] = 18*60 + 1
			})
		}},
	}
	for _, tc := range damages {
		t.Run(tc.name, func(t *testing.T) {
			source, cachePath := storedIndexFixture(t)
			tc.damage(t, cachePath)
			log := captureIndexLog(t)

			idx, err := LoadFromPath(cachePath)
			if idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadFromPath = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			if idx, err := LoadForSource(source); idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadForSource = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			logged := log.String()
			if !strings.Contains(logged, "index_unreadable") || !strings.Contains(logged, filepath.Base(cachePath)) {
				t.Errorf("no warning naming the damaged index; log:\n%s", logged)
			}
		})
	}
}

// A time section at the edges of what the checks accept still loads:
// first on line 1, last on the file's last line, a zone 18 hours east.
func TestLoadFromPath_TimeSectionAtTheEdgesLoads(t *testing.T) {
	_, cachePath := storedIndexFixture(t)
	rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ int) {
		ti["has_zone"] = true
		ti["timestamped_lines"] = 200
		ti["first"] = map[string]any{"ms": 1, "line": 1, "offset": 0}
		ti["last"] = map[string]any{"ms": 2, "line": 200, "offset": 0}
		ti["first_zone_offset_minutes"] = -18 * 60
	})
	if idx, err := LoadFromPath(cachePath); idx == nil || idx.TimeIndex == nil {
		t.Fatalf("LoadFromPath = %v, %v; want the index with its time section", idx, err)
	}
}

// A missing index and one of another format version are ordinary
// misses: absent, and not worth a warning.
func TestLoadFromPath_OrdinaryMissIsNotLogged(t *testing.T) {
	misses := []struct {
		name  string
		setUp func(t *testing.T, cachePath string)
	}{
		{"missing", func(t *testing.T, cachePath string) {
			if err := os.Remove(cachePath); err != nil {
				t.Fatalf("remove index: %v", err)
			}
		}},
		{"other version", func(t *testing.T, cachePath string) {
			body, err := json.Marshal(map[string]any{"version": Version - 1})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(cachePath, body, 0o600); err != nil {
				t.Fatalf("write index: %v", err)
			}
		}},
	}
	for _, tc := range misses {
		t.Run(tc.name, func(t *testing.T) {
			_, cachePath := storedIndexFixture(t)
			tc.setUp(t, cachePath)
			log := captureIndexLog(t)

			if idx, err := LoadFromPath(cachePath); idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadFromPath = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			if strings.Contains(log.String(), "index_unreadable") {
				t.Errorf("an ordinary miss was logged as unreadable:\n%s", log.String())
			}
		})
	}
}
