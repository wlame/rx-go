package output

import "strings"

// Quote returns a shell-safe rendition of arg, matching Python's
// shlex.quote exactly (Decision 5.8).
//
// Algorithm, verbatim from Python's stdlib:
//  1. Empty string → "”" (two single quotes).
//  2. If every character is in the "safe" set
//     [A-Za-z0-9@%+=:,./_-], return arg unmodified.
//  3. Otherwise wrap in single quotes, replacing every embedded '
//     with the sequence '"'"' — closes the single-quoted string,
//     opens a double-quoted one containing just one ', then re-opens
//     the single-quoted string.
//
// Example: Quote("it's a test") → `'it'"'"'s a test'`.
//
// Go parity with Python: the regex used by Python's shlex is
// re.compile(r'[^\w@%+=:,./-]', re.ASCII).search — with re.ASCII, \w
// is just [A-Za-z0-9_]. We inline that test below to stay dependency-free.
func Quote(arg string) string {
	if arg == "" {
		return "''"
	}
	if hasOnlySafeChars(arg) {
		return arg
	}
	// Replace every ' with '"'"' then wrap the whole thing in '...'.
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// hasOnlySafeChars reports whether every rune in arg is in the POSIX
// "safe" character set used by shlex.quote: ASCII letters, digits,
// underscore, and the literal punctuation @ % + = : , . / -.
//
// Non-ASCII runes (e.g. Unicode) are treated as unsafe, matching
// Python's re.ASCII behavior.
func hasOnlySafeChars(s string) bool {
	for _, r := range s {
		if !isSafeRune(r) {
			return false
		}
	}
	return true
}

// isSafeRune reports whether r is in the shlex-safe set.
func isSafeRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '_', '@', '%', '+', '=', ':', ',', '.', '/', '-':
		return true
	}
	return false
}
