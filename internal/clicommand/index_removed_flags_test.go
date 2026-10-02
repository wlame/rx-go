package clicommand

import (
	"errors"
	"io"
	"testing"
)

// The removed analysis flags are refused by the real `rx index` command
// before it does anything: the action never runs, and the error is a
// usage error (exit 2) whose line is already printed. Any other command
// line reaches the action.
func TestIndexCommand_RemovedFlagsNeverReachTheAction(t *testing.T) {
	cases := []struct {
		args       []string
		wantRefuse bool
	}{
		{[]string{"/tmp/app.log", "--analyze"}, true},
		{[]string{"/tmp/app.log", "-a"}, true},
		{[]string{"--analyze"}, true},
		{[]string{"/tmp/app.log", "--analyze-window-lines=256"}, true},
		{[]string{"/tmp/app.log", "--analyze-window-lines="}, true},
		{[]string{"/tmp/app.log", "--json", "--threshold=0"}, false},
	}
	for _, tc := range cases {
		ran := false
		cmd := newIndexCommand(io.Discard, func(io.Writer, indexParams) error {
			ran = true
			return nil
		})
		cmd.SetArgs(tc.args)
		cmd.SetErr(io.Discard)

		err := cmd.Execute()

		var exitErr *ExitError
		refused := errors.As(err, &exitErr) && exitErr.Code == ExitUsageError && exitErr.Reported
		if refused != tc.wantRefuse || ran == tc.wantRefuse {
			t.Errorf("%v: refused %v (err %v), action ran %v; want refused %v",
				tc.args, refused, err, ran, tc.wantRefuse)
		}
	}
}
