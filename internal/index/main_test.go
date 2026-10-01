package index

import (
	"testing"

	"github.com/wlame/rx-go/internal/testutil/isolatedcache"
)

// TestMain points RX_CACHE_DIR at a temporary directory for every test in
// this package, so no test reads or writes the user's real rx cache.
// Binaries the tests start inherit the variable.
func TestMain(m *testing.M) {
	isolatedcache.Main(m)
}
