package config

import (
	"os"
	"path/filepath"
)

// DebugMode reports whether RX_DEBUG is set to a truthy value. When on,
// the trace engine writes .debug_* artifacts under DebugDir().
func DebugMode() bool {
	return GetBoolEnv("RX_DEBUG", false)
}

// DebugDir returns the directory for debug artifacts.
//
// Precedence:
//  1. $RX_DEBUG_DIR (explicit override)
//  2. $TMPDIR/rx-debug     (spec §9 intentional deviation 4: Go writes
//     debug files to TMPDIR to keep the user's CWD clean, whereas Python
//     writes them to the current working directory)
//  3. /tmp/rx-debug        (fallback when TMPDIR is unset)
func DebugDir() string {
	if v := os.Getenv("RX_DEBUG_DIR"); v != "" {
		return v
	}
	tmp := os.TempDir()
	return filepath.Join(tmp, "rx-debug")
}
