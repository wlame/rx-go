package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/wlame/rx-go/internal/config"
)

// CachePath returns the on-disk location for one analyzer's output for
// one source file, per decision 5.3:
//
//	<cache-base>/analyzers/<name>/v<version>/<file-sha256-prefix>_<basename>.json
//
// Using both a content-addressable hash prefix AND the basename keeps
// cache files self-describing: a human debugging a failure can glance
// at the filename and know which file's output it is.
//
// The hash is sha256(abs_path)[:16] — same as Python's trace cache
// path_hash (spec §5.2) for cross-format consistency.
func CachePath(name, version, sourcePath string) string {
	sum := sha256.Sum256([]byte(sourcePath))
	prefix := hex.EncodeToString(sum[:8]) // 16 hex chars = 8 bytes
	base := filepath.Base(sourcePath)
	return filepath.Join(
		config.GetAnalyzerCacheDir(name, version),
		prefix+"_"+base+".json",
	)
}
