package logchain

import (
	"testing"

	"github.com/wlame/rx-go/internal/testutil/isolatedcache"
)

// TestMain points RX_CACHE_DIR at a temporary directory for every test in
// this package: List peeks at the line-index cache for is_indexed, and no
// test may read or write the user's real rx cache.
func TestMain(m *testing.M) {
	isolatedcache.Main(m)
}
