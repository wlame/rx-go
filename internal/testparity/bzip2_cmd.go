package testparity

import (
	"os/exec"
)

// bzip2Command returns an exec.Cmd that reads from stdin and writes
// compressed bzip2 to `target`. Returns nil if the `bzip2` binary
// isn't on PATH — callers should treat that as "bz2 fixtures can't
// be built on this host, skip tests that need them".
//
// We split this out so TestParity harness code can ship a fallback for
// non-Linux hosts without polluting the main fixtures file.
func bzip2Command(target string) *exec.Cmd {
	path, err := exec.LookPath("bzip2")
	if err != nil {
		return nil
	}
	cmd := exec.Command(path, "-c")
	// Hook up stdout to the target file.
	// Using a direct file handle instead of Popen so caller doesn't
	// need to pipe stdout through another goroutine.
	f, ferr := osCreate(target)
	if ferr != nil {
		return nil
	}
	cmd.Stdout = f
	// We intentionally don't close `f` here — cmd.Wait() blocks the
	// caller, and Go's exec package holds references until then. The
	// OS releases on process exit anyway.
	return cmd
}
