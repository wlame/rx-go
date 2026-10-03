package config

import "testing"

// The two per-line bounds of a trace answer take a positive whole
// number from the environment and fall back to their defaults
// otherwise: a bound of zero or below would drop every line's text, so
// it is never what the operator meant.
func TestLineLimits_ReadPositiveValuesAndFallBackOtherwise(t *testing.T) {
	cases := []struct {
		name          string
		value         string
		wantLineBytes int
		wantSubs      int
	}{
		{"unset", "", DefaultMaxLineTextBytes, DefaultMaxSubmatchesPerLine},
		{"positive", "4096", 4096, 4096},
		{"one", "1", 1, 1},
		{"zero", "0", DefaultMaxLineTextBytes, DefaultMaxSubmatchesPerLine},
		{"negative", "-5", DefaultMaxLineTextBytes, DefaultMaxSubmatchesPerLine},
		{"not a number", "lots", DefaultMaxLineTextBytes, DefaultMaxSubmatchesPerLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RX_MAX_LINE_TEXT_BYTES", tc.value)
			t.Setenv("RX_MAX_SUBMATCHES_PER_LINE", tc.value)

			if got := MaxLineTextBytes(); got != tc.wantLineBytes {
				t.Errorf("MaxLineTextBytes() = %d, want %d", got, tc.wantLineBytes)
			}
			if got := MaxSubmatchesPerLine(); got != tc.wantSubs {
				t.Errorf("MaxSubmatchesPerLine() = %d, want %d", got, tc.wantSubs)
			}
		})
	}
}

// The defaults are documented in docs/configuration.md; changing one is
// a visible change to every trace answer.
func TestLineLimits_Defaults(t *testing.T) {
	if DefaultMaxLineTextBytes != 1<<20 {
		t.Errorf("DefaultMaxLineTextBytes = %d, want 1 MiB", DefaultMaxLineTextBytes)
	}
	if DefaultMaxSubmatchesPerLine != 10_000 {
		t.Errorf("DefaultMaxSubmatchesPerLine = %d, want 10000", DefaultMaxSubmatchesPerLine)
	}
}
