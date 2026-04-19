package output

import "testing"

func TestQuote(t *testing.T) {
	// Ports from rx-python/tests/test_cli_command_builder.py::TestShellQuote.
	// The "want" values are what Python's shlex.quote returns for the
	// same input — verified by running the same input through Python.
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Safe values — no quoting needed.
		{"simple alnum", "hello", "hello"},
		{"with dot", "file.log", "file.log"},
		{"with dash", "some-id-123", "some-id-123"},
		{"with slash", "/var/log/app.log", "/var/log/app.log"},
		{"with underscore", "a_b_c", "a_b_c"},
		{"with @", "foo@bar", "foo@bar"},
		{"with percent", "a%b", "a%b"},
		{"with plus", "a+b", "a+b"},
		{"with equals", "a=b", "a=b"},
		{"with colon", "a:b", "a:b"},
		{"with comma", "a,b", "a,b"},

		// Unsafe — require single-quoting.
		{"empty", "", "''"},
		{"space", "hello world", "'hello world'"},
		{"path with space", "/path/to/my file.log", "'/path/to/my file.log'"},
		{"dollar", "file$name", "'file$name'"},
		{"semicolon", "a;b", "'a;b'"},
		{"pipe", "a|b", "'a|b'"},
		{"ampersand", "a&b", "'a&b'"},
		{"star", "error.*failed", "'error.*failed'"},
		{"parens", "foo(bar)", "'foo(bar)'"},
		{"brackets", "foo[bar]", "'foo[bar]'"},
		{"hash", "a#b", "'a#b'"},
		{"question", "a?b", "'a?b'"},
		{"bang", "a!b", "'a!b'"},

		// Embedded single quotes — use the '"'"' trick.
		{"single quote", "it's", `'it'"'"'s'`},
		{"multiple single quotes", "can't won't", `'can'"'"'t won'"'"'t'`},
		{"only single quote", "'", `''"'"''`},

		// Double quotes — still wrapped in single quotes.
		{"double quotes", `say "hello"`, `'say "hello"'`},

		// Newline and tab.
		{"newline", "a\nb", "'a\nb'"},
		{"tab", "a\tb", "'a\tb'"},

		// Unicode — non-ASCII must be quoted because re.ASCII makes
		// shlex treat everything outside ASCII as unsafe.
		{"unicode japanese", "日本語", "'日本語'"},
		{"unicode cyrillic", "файл", "'файл'"},

		// Edge cases.
		{"only space", " ", "' '"},
		{"only dashes", "---", "---"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Quote(tc.input)
			if got != tc.want {
				t.Errorf("Quote(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestQuote_IdempotentOnSafe(t *testing.T) {
	// Re-quoting a safe value is a no-op; re-quoting an already-quoted
	// value wraps it AGAIN (Python's behavior — shlex.quote isn't
	// idempotent on already-escaped strings).
	safe := "file.log"
	if got := Quote(Quote(safe)); got != safe {
		t.Errorf("double-quote on safe input: got %q, want %q", got, safe)
	}
	// Whereas an unsafe input re-wraps:
	unsafe := "a b"
	first := Quote(unsafe) // "'a b'"
	second := Quote(first) // should re-wrap because "'" is unsafe
	if second == first {
		t.Errorf("expected re-wrap on unsafe-quote, got %q", second)
	}
}
