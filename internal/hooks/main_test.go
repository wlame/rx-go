package hooks

import (
	"os"
	"testing"
)

// TestMain opts this package's tests into internal hook targets.
//
// Almost every test here points a hook at an httptest server, which
// listens on 127.0.0.1. The dial guard refuses loopback by default, so
// without this the tests would be exercising the guard rather than the
// dispatcher. RX_ALLOW_INTERNAL_HOOKS is the same opt-in an operator
// uses to send hooks to a local collector, so the tests run in a real
// supported configuration rather than a special test mode.
//
// Tests that need the guard active set the variable to "false"
// themselves with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
