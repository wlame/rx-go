package clicommand

// The text after "Error: " is a sentence a person reads, and rx-python
// has always started it with a capital. Go error strings are lower case
// by convention, so the same mistake was reported as
//
//	rx-go       Error: path not found: /tmp/x.log
//	rx-python   Error: Path not found: /tmp/x.log
//
// which a wrapper matching on stderr sees as two different failures.
// exitWithError now capitalizes for display only; the wrapped error keeps
// the lower-case form that errors.Is callers and the HTTP layer see.

import (
	"bytes"
	"strings"
	"testing"
)

func TestExitWithError_CapitalizesTheDisplayedMessage(t *testing.T) {
	cases := []struct {
		name  string
		msg   string
		want  string
		wrapd string // what the wrapped error must still say
	}{
		{
			name:  "lower case Go error is capitalized",
			msg:   "path not found: /tmp/x.log",
			want:  "Error: Path not found: /tmp/x.log\n",
			wrapd: "path not found: /tmp/x.log",
		},
		{
			name:  "an already capitalized message is left alone",
			msg:   "Access denied: path '/x' is outside all search roots: '/root'",
			want:  "Error: Access denied: path '/x' is outside all search roots: '/root'\n",
			wrapd: "Access denied: path '/x' is outside all search roots: '/root'",
		},
		{
			// A flag name must not be reshaped into "--Max-results".
			name:  "a message starting with a flag is left alone",
			msg:   "--max-results is required when --hook-on-match is configured.",
			want:  "Error: --max-results is required when --hook-on-match is configured.\n",
			wrapd: "--max-results is required when --hook-on-match is configured.",
		},
		{
			// Only the first rune changes, so a lower-case tool name
			// further into the sentence survives.
			name:  "only the first letter changes",
			msg:   "ripgrep (rg) is not installed or not on PATH",
			want:  "Error: Ripgrep (rg) is not installed or not on PATH\n",
			wrapd: "ripgrep (rg) is not installed or not on PATH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := exitWithError(&buf, ExitGenericError, "%s", tc.msg)

			if got := buf.String(); got != tc.want {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
			if err.Err.Error() != tc.wrapd {
				t.Errorf("wrapped error is %q, want the unchanged %q", err.Err.Error(), tc.wrapd)
			}
		})
	}
}

// An empty message must not panic, and a message whose first rune is
// multi-byte must not be cut in half.
func TestExitWithError_HandlesEmptyAndMultiByteMessages(t *testing.T) {
	var empty bytes.Buffer
	exitWithError(&empty, ExitGenericError, "%s", "")
	if got := empty.String(); got != "Error: \n" {
		t.Errorf("empty message printed %q, want %q", got, "Error: \n")
	}

	var multiByte bytes.Buffer
	exitWithError(&multiByte, ExitGenericError, "%s", "ünreadable path: /tmp/x")
	got := multiByte.String()
	if !strings.HasPrefix(got, "Error: Ünreadable path: ") {
		t.Errorf("multi-byte first rune printed %q", got)
	}
}
