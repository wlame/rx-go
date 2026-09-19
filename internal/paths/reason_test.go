package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// A reason never carries a path, a root or an error's own text: only the
// fixed wording for what kind of failure it was.
func TestFailureReasonGivesFixedWordingWithoutPaths(t *testing.T) {
	const secret = "/outside/secret/target.log"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"permission", &fs.PathError{Op: "open", Path: secret, Err: syscall.EACCES}, ReasonPermissionDenied},
		{"missing link target", &fs.PathError{Op: "lstat", Path: secret, Err: syscall.ENOENT}, ReasonNotFound},
		{"outside the roots", &ErrPathOutsideRoots{Path: secret, Roots: []string{"/srv/logs"}}, ReasonOutsideRoots},
		{"hidden", &ErrHiddenPath{Path: secret, Component: ".secret"}, ReasonHidden},
		{"changed", fmt.Errorf("%w: %s", ErrFileChanged, secret), ReasonChanged},
		{"link loop", &fs.PathError{Op: "lstat", Path: secret, Err: syscall.ELOOP}, ReasonUnreadable},
		{"anything else", errors.New("read " + secret + ": input/output error"), ReasonUnreadable},
	}
	for _, tc := range cases {
		got := FailureReason(tc.err)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "/srv/logs") {
			t.Errorf("%s: %q names a path", tc.name, got)
		}
	}
}
