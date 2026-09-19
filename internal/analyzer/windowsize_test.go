package analyzer

import (
	"os"
	"strconv"
	"testing"
)

// envWindowLinesVar is the variable the window size falls back to.
const envWindowLinesVar = "RX_ANALYZE_WINDOW_LINES"

// TestResolveWindowLines_Precedence verifies the full precedence chain:
// URL > CLI > env > default. Each row sets exactly the inputs that
// matter for the branch it exercises and asserts the expected output.
func TestResolveWindowLines_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		cli      int
		url      int
		env      string // "" means env is unset; "-" means explicitly-empty value
		setEnv   bool
		expected int
	}{
		{
			name:     "default_when_nothing_set",
			cli:      0,
			url:      0,
			setEnv:   false,
			expected: defaultWindowLines,
		},
		{
			name:     "env_used_when_no_cli_or_url",
			cli:      0,
			url:      0,
			env:      "256",
			setEnv:   true,
			expected: 256,
		},
		{
			name:     "cli_beats_env",
			cli:      64,
			url:      0,
			env:      "256",
			setEnv:   true,
			expected: 64,
		},
		{
			name:     "url_beats_cli_and_env",
			cli:      64,
			url:      512,
			env:      "256",
			setEnv:   true,
			expected: 512,
		},
		{
			name:     "url_beats_cli_without_env",
			cli:      64,
			url:      512,
			setEnv:   false,
			expected: 512,
		},
		{
			name:     "cli_zero_falls_through_to_env",
			cli:      0,
			url:      0,
			env:      "99",
			setEnv:   true,
			expected: 99,
		},
		{
			name:     "negative_cli_treated_as_unset",
			cli:      -5,
			url:      0,
			env:      "200",
			setEnv:   true,
			expected: 200,
		},
		{
			name:     "negative_url_treated_as_unset",
			cli:      64,
			url:      -1,
			setEnv:   false,
			expected: 64,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv(envWindowLinesVar, tc.env)
			} else {
				// t.Setenv with empty-string still defines the variable;
				// we need Unsetenv semantics. Use os.Unsetenv via env
				// helper inlined here to keep the test table compact.
				t.Setenv(envWindowLinesVar, "") // shadow any process-level value
				if err := unsetEnvForTest(t, envWindowLinesVar); err != nil {
					t.Fatalf("unsetEnvForTest: %v", err)
				}
			}
			got := ResolveWindowLines(tc.cli, tc.url)
			if got != tc.expected {
				t.Errorf("ResolveWindowLines(cli=%d, url=%d, env=%q) = %d, want %d",
					tc.cli, tc.url, tc.env, got, tc.expected)
			}
		})
	}
}

// TestResolveWindowLines_Clamping covers values outside [1, maxWindowLines]
// from every source, confirming the clamp is applied uniformly.
func TestResolveWindowLines_Clamping(t *testing.T) {
	t.Run("url_over_cap_clamps_down", func(t *testing.T) {
		got := ResolveWindowLines(0, maxWindowLines+999)
		if got != maxWindowLines {
			t.Errorf("got %d, want %d", got, maxWindowLines)
		}
	})
	t.Run("cli_over_cap_clamps_down", func(t *testing.T) {
		t.Setenv(envWindowLinesVar, "")
		got := ResolveWindowLines(maxWindowLines+1, 0)
		if got != maxWindowLines {
			t.Errorf("got %d, want %d", got, maxWindowLines)
		}
	})
	t.Run("env_over_cap_clamps_down", func(t *testing.T) {
		t.Setenv(envWindowLinesVar, strconv.Itoa(maxWindowLines+100))
		got := ResolveWindowLines(0, 0)
		if got != maxWindowLines {
			t.Errorf("got %d, want %d", got, maxWindowLines)
		}
	})
	t.Run("url_of_one_stays_at_one", func(t *testing.T) {
		got := ResolveWindowLines(0, 1)
		if got != 1 {
			t.Errorf("got %d, want 1", got)
		}
	})
}

// TestResolveWindowLines_InvalidEnvKeepsTheDefault covers the values
// of RX_ANALYZE_WINDOW_LINES the setting does not accept: each gives
// the default window when no flag or request value is set.
func TestResolveWindowLines_InvalidEnvKeepsTheDefault(t *testing.T) {
	for _, value := range []string{"", "abc", "3.14", "0", "-10", " 10", "10foo"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(envWindowLinesVar, value)
			if got := ResolveWindowLines(0, 0); got != defaultWindowLines {
				t.Errorf("RX_ANALYZE_WINDOW_LINES=%q gives %d, want %d", value, got, defaultWindowLines)
			}
		})
	}
	t.Setenv(envWindowLinesVar, "321")
	if got := ResolveWindowLines(0, 0); got != 321 {
		t.Errorf("RX_ANALYZE_WINDOW_LINES=321 gives %d", got)
	}
}

// TestClampWindowLines covers the helper in isolation so we have
// explicit coverage of the [1, maxWindowLines] boundaries.
func TestClampWindowLines(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-100, 1},
		{-1, 1},
		{0, 1},
		{1, 1},
		{128, 128},
		{maxWindowLines, maxWindowLines},
		{maxWindowLines + 1, maxWindowLines},
		{1 << 30, maxWindowLines},
	}
	for _, c := range cases {
		got := clampWindowLines(c.in)
		if got != c.want {
			t.Errorf("clampWindowLines(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// unsetEnvForTest is a tiny helper to cleanly remove an environment
// variable inside a subtest. t.Setenv cannot "unset" — it always
// writes a value — so we delegate to os.Unsetenv.
//
// t.Setenv is called just before this helper in each test site; that
// call records the original value and restores it after the test, so
// the test-local Unsetenv here doesn't leak to later tests.
func unsetEnvForTest(t *testing.T, key string) error {
	t.Helper()
	return os.Unsetenv(key)
}
