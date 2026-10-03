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
