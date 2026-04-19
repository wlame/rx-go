package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetCacheBase_Default(t *testing.T) {
	os.Unsetenv("RX_CACHE_DIR")
	os.Unsetenv("XDG_CACHE_HOME")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this platform")
	}
	got := GetCacheBase()
	want := filepath.Join(home, ".cache", "rx")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetCacheBase_RXCacheDirTakesPriority(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RX_CACHE_DIR", tmp)
	t.Setenv("XDG_CACHE_HOME", "/should/be/ignored")
	got := GetCacheBase()
	want := filepath.Join(tmp, "rx")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetCacheBase_XDGCacheHome(t *testing.T) {
	tmp := t.TempDir()
	os.Unsetenv("RX_CACHE_DIR")
	t.Setenv("XDG_CACHE_HOME", tmp)
	got := GetCacheBase()
	want := filepath.Join(tmp, "rx")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetCacheBase_PythonParity_RxCacheDirAppendsRx(t *testing.T) {
	// Python's get_rx_cache_base always appends /rx, even when RX_CACHE_DIR
	// is explicitly set. This is the most-frequently-surprising behavior;
	// pinning it with its own test so nobody "helpfully" removes the
	// append.
	t.Setenv("RX_CACHE_DIR", "/custom/cache")
	got := GetCacheBase()
	want := filepath.Join("/custom/cache", "rx")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCacheSubdirectories(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RX_CACHE_DIR", tmp)
	base := filepath.Join(tmp, "rx")

	cases := []struct {
		name string
		got  func() string
		want string
	}{
		{"index", GetIndexCacheDir, filepath.Join(base, "indexes")},
		{"trace", GetTraceCacheDir, filepath.Join(base, "trace_cache")},
		{"frontend", GetFrontendCacheDir, filepath.Join(base, "frontend")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetAnalyzerCacheDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RX_CACHE_DIR", tmp)
	got := GetAnalyzerCacheDir("drain3", "1.2.3")
	want := filepath.Join(tmp, "rx", "analyzers", "drain3", "v1.2.3")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetCacheBase_FallbackWithNoHome(t *testing.T) {
	// Hard to exercise because os.UserHomeDir is hard to fail deterministically.
	// Instead, verify that the default path construction is well-formed.
	os.Unsetenv("RX_CACHE_DIR")
	os.Unsetenv("XDG_CACHE_HOME")
	got := GetCacheBase()
	if got == "" {
		t.Error("expected non-empty path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

// End-to-end check: all helpers respect the same RX_CACHE_DIR override.
func TestAllHelpersShareBase(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RX_CACHE_DIR", tmp)
	base := GetCacheBase()
	for _, path := range []string{
		GetIndexCacheDir(),
		GetTraceCacheDir(),
		GetFrontendCacheDir(),
		GetAnalyzerCacheDir("foo", "1.0.0"),
	} {
		if !hasPrefix(path, base) {
			t.Errorf("helper returned %q, not under base %q", path, base)
		}
	}
}

func hasPrefix(path, prefix string) bool {
	if len(path) < len(prefix) {
		return false
	}
	return path[:len(prefix)] == prefix
}
