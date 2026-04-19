package config

import (
	"os"
	"path/filepath"
)

// GetCacheBase returns the base directory for rx cache files.
//
// Resolution order (Decision 5.11):
//  1. $RX_CACHE_DIR/rx         (explicit override; Python appends "rx" too)
//  2. $XDG_CACHE_HOME/rx       (freedesktop spec default)
//  3. $HOME/.cache/rx          (ultimate fallback)
//
// The path is returned absolute but is NOT created on disk — callers
// that need to write into it must mkdir themselves.
//
// Python parity: rx-python/src/rx/utils.py::get_rx_cache_base. Both
// Python and Go append a literal "rx" segment in all three cases, so
// RX_CACHE_DIR=/tmp yields /tmp/rx, not /tmp.
func GetCacheBase() string {
	if v := os.Getenv("RX_CACHE_DIR"); v != "" {
		return filepath.Join(v, "rx")
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "rx")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Very unlikely on supported platforms; degrade gracefully to /tmp.
		return filepath.Join(os.TempDir(), "rx")
	}
	return filepath.Join(home, ".cache", "rx")
}

// GetIndexCacheDir returns {base}/indexes — where UnifiedFileIndex JSONs live.
func GetIndexCacheDir() string {
	return filepath.Join(GetCacheBase(), "indexes")
}

// GetTraceCacheDir returns {base}/trace_cache — where trace cache JSONs live.
func GetTraceCacheDir() string {
	return filepath.Join(GetCacheBase(), "trace_cache")
}

// GetFrontendCacheDir returns {base}/frontend — where the rx-viewer SPA
// gets unpacked.
func GetFrontendCacheDir() string {
	return filepath.Join(GetCacheBase(), "frontend")
}

// GetAnalyzerCacheDir returns the per-analyzer cache directory scheme
// from Decision 5.3:
//
//	{base}/analyzers/<name>/v<version>/
//
// Each analyzer gets its own namespace so upgrading one detector can't
// invalidate the others' cached output. The caller typically appends
// <file-hash>.json to get the final path.
func GetAnalyzerCacheDir(name, version string) string {
	return filepath.Join(GetCacheBase(), "analyzers", name, "v"+version)
}
