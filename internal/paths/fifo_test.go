package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// makeFIFO creates a named pipe at path, skipping the test where the
// filesystem cannot hold one.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

// withinBudget runs f and fails the test when it has not returned after
// a few seconds: opening a named pipe for reading blocks until a writer
// comes, so a check that opens one would never return.
func withinBudget(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a named pipe")
	}
}

// A pinned path that is not a regular file or a directory (a named
// pipe, a socket, a device) is refused by Open without being opened,
// with an error wrapping ErrNotRegularFile, and its reason says so.
func TestOpenRefusesANamedPipeWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe.log")
	makeFIFO(t, path)
	src, err := Pin(path)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	withinBudget(t, func() {
		f, err := src.Open()
		if err == nil {
			_ = f.Close()
		}
		if !errors.Is(err, ErrNotRegularFile) {
			t.Errorf("Open: got %v, want ErrNotRegularFile", err)
		}
		if got := FailureReason(err); got != ReasonNotRegularFile {
			t.Errorf("reason: got %q, want %q", got, ReasonNotRegularFile)
		}
	})
}

// A path checked as a regular file and swapped for a named pipe before
// the read is opened without blocking and refused as changed.
func TestOpenOfAFileSwappedForANamedPipeDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("LINE 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src, err := Pin(path)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	makeFIFO(t, path)
	withinBudget(t, func() {
		f, err := src.Open()
		if err == nil {
			_ = f.Close()
			t.Error("Open of a swapped path succeeded")
		}
	})
}

// The file type is part of a pinned file's identity, its permissions
// are not: a file whose mode was changed since the check is still the
// checked file and opens.
func TestOpenOfAFileWhosePermissionsChangedReadsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("LINE 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src, err := Pin(path)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	f, err := src.Open()
	if err != nil {
		t.Fatalf("Open after a chmod: %v", err)
	}
	_ = f.Close()
	if _, err := src.Stat(); err != nil {
		t.Errorf("Stat after a chmod: %v", err)
	}
}

// A directory walk refuses a named pipe it meets, with the reason, and
// never reports it as a file to read.
func TestWalkRefusesANamedPipe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.log"), []byte("LINE 1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pipe := filepath.Join(dir, "pipe.log")
	makeFIFO(t, pipe)
	entries, err := WalkDir(dir, true)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Path == pipe {
			found = true
			if entry.Refused != ReasonNotRegularFile {
				t.Errorf("pipe: refused %q, want %q", entry.Refused, ReasonNotRegularFile)
			}
		}
	}
	if !found {
		t.Errorf("the walk did not report the pipe: %+v", entries)
	}
}
