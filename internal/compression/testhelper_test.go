package compression

import (
	"os/exec"
)

// lookPath wraps exec.LookPath for test helpers.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// execCommand wraps exec.Command for test helpers.
func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
