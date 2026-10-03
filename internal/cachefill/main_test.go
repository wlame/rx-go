package cachefill

import (
	"testing"

	"github.com/wlame/rx-go/internal/testutil/isolatedcache"
)

// TestMain points RX_CACHE_DIR at a temporary directory for every test
// in this package, so no test can reach the user's real rx cache.
func TestMain(m *testing.M) {
	isolatedcache.Main(m)
}
