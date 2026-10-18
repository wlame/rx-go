package samples

import (
	"errors"
	"testing"
)

// A negative context on either side is a mistake in the request, not a
// value with a meaning: the callers resolve their own -1 sentinels
// before they get here, so Resolve refuses it as a usage error and
// reads nothing.
func TestResolve_NegativeContextIsRefused(t *testing.T) {
	plain, _ := writeGzipFixture(t, 30)

	for name, req := range map[string]Request{
		"before": {Path: plain, IndexLoader: NoIndex, Lines: []OffsetOrRange{{Start: 10}}, BeforeContext: -1, AfterContext: 0},
		"after":  {Path: plain, IndexLoader: NoIndex, Lines: []OffsetOrRange{{Start: 10}}, BeforeContext: 0, AfterContext: -2},
	} {
		resp, err := Resolve(t.Context(), req)
		if !errors.Is(err, ErrNegativeContext) || !IsUsageError(err) {
			t.Errorf("%s: Resolve = %v, %v; want ErrNegativeContext, a usage error", name, resp, err)
		}
	}
}
