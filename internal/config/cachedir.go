package config

import (
	"os"
	"path/filepath"
)

// GetCacheBase returns the base directory for rx cache files.
//
// Resolution order:
//  1. $RX_CACHE_DIR/rx         (explicit override; Python appends "rx" too)
//  2. $XDG_CACHE_HOME/rx       (freedesktop spec default)
//  3. $HOME/.cache/rx          (ultimate fallback)
//
// The path is returned absolute (see absoluteCacheDir) but is NOT
// created on disk — callers that need to write into it must mkdir
// themselves.
//
// Python parity: rx-python/src/rx/utils.py::get_rx_cache_base. Both
// Python and Go append a literal "rx" segment in all three cases, so
// RX_CACHE_DIR=/tmp yields /tmp/rx, not /tmp.
func GetCacheBase() string {
	if v := os.Getenv("RX_CACHE_DIR"); v != "" {
		return absoluteCacheDir(filepath.Join(v, "rx"))
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return absoluteCacheDir(filepath.Join(v, "rx"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Very unlikely on supported platforms; degrade gracefully to /tmp.
		return filepath.Join(os.TempDir(), "rx")
	}
	return filepath.Join(home, ".cache", "rx")
}

// absoluteCacheDir makes a cache directory taken from a variable
// absolute against the current directory. A relative value would
// otherwise be looked up again against whatever directory each file
// operation runs from, and /health would report a path that says
// nothing about where the cache is. rx never changes its working
// directory, so every call in one process resolves the value to the
// same directory: the one the process started in.
//
// When the current directory cannot be found (it was removed), the
// value is returned as given; every cache operation then fails or
// misses, which costs speed and never an answer.
func absoluteCacheDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
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
