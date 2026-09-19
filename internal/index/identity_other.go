//go:build !linux && !darwin

package index

import (
	"os"
)

// sourceIdentity has no portable implementation outside linux and
// darwin, the two platforms rx builds for. Reporting ok=false makes
// every caller fall back to comparing size and mtime, which is what rx
// did before inode and ctime were recorded.
func sourceIdentity(os.FileInfo) (statIdentity, bool) {
	return statIdentity{}, false
}
