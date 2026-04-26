package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func init() {
	// Every test below assumes the package starts with zero search roots.
	// Ordering between test functions is not guaranteed but init runs
	// once before any test — that's enough for the first test to be
	// clean. Individual tests use t.Cleanup(Reset) to stay isolated.
	Reset()
}

func TestSetSearchRoots_Basic(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	roots := GetSearchRoots()
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	// macOS symlinks /tmp to /private/tmp; the resolved form is what we store.
	if !strings.HasSuffix(roots[0], filepath.Base(tmp)) {
		t.Errorf("expected root to end with %q, got %q", filepath.Base(tmp), roots[0])
	}
}

func TestSetSearchRoots_RejectsNonexistent(t *testing.T) {
	t.Cleanup(Reset)
	err := SetSearchRoots([]string{"/this/really/does/not/exist"})
	if err == nil {
		t.Error("expected error for nonexistent root")
	}
}

func TestSetSearchRoots_RejectsFile(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	f := filepath.Join(tmp, "regular.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{f}); err == nil {
		t.Error("expected error for file-not-dir root")
	}
}

func TestSetSearchRoots_RejectsEmpty(t *testing.T) {
	t.Cleanup(Reset)
	err := SetSearchRoots([]string{""})
	if err == nil {
		t.Error("expected error for empty string root")
	}
}

func TestSetSearchRoots_DeduplicatesResolvedPaths(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	// Both "tmp" and "tmp/." resolve to the same path.
	if err := SetSearchRoots([]string{tmp, filepath.Join(tmp, ".")}); err != nil {
		t.Fatalf("SetSearchRoots: %v", err)
	}
	roots := GetSearchRoots()
	if len(roots) != 1 {
		t.Errorf("expected dedup to 1, got %d: %v", len(roots), roots)
	}
}

func TestValidate_NoRoots_ReturnsError(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	_, err := ValidatePathWithinRoots("/tmp/anything")
	if !errors.Is(err, ErrNoSearchRootsConfigured) {
		t.Errorf("expected ErrNoSearchRootsConfigured, got %v", err)
	}
}

func TestValidate_InsideRoot(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}

	// Create a file inside the root.
	inside := filepath.Join(tmp, "foo.log")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ValidatePathWithinRoots(inside)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	// The returned path is the absolute (but non-canonical) form —
	// downstream consumers (cache hashers, os.Open) rely on this shape
	// being stable, so symlink resolution happens only internally for
	// the sandbox prefix check.
	want, _ := filepath.Abs(inside)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidate_OutsideRoot(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	outside := "/etc/hosts"
	_, err := ValidatePathWithinRoots(outside)
	if err == nil {
		t.Fatalf("expected error for path outside sandbox")
	}
	var typedErr *ErrPathOutsideRoots
	if !errors.As(err, &typedErr) {
		t.Errorf("expected *ErrPathOutsideRoots, got %T: %v", err, err)
	}
	if typedErr.Path != outside {
		t.Errorf("typedErr.Path = %q, want %q", typedErr.Path, outside)
	}
	if len(typedErr.Roots) != 1 {
		t.Errorf("typedErr.Roots has %d entries, want 1", len(typedErr.Roots))
	}
}

func TestValidate_ErrorMessageFormat(t *testing.T) {
	// Python-compat format:
	//   Access denied: path '<p>' is outside all search roots: '<r1>', '<r2>'
	t.Cleanup(Reset)
	e := &ErrPathOutsideRoots{
		Path:  "/etc/hosts",
		Roots: []string{"/var/log", "/tmp"},
	}
	got := e.Error()
	want := "Access denied: path '/etc/hosts' is outside all search roots: '/var/log', '/tmp'"
	if got != want {
		t.Errorf("\ngot:  %q\nwant: %q", got, want)
	}
}

func TestValidate_DotDotEscape(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	// Crafting a path that tries to escape via ../..
	escape := filepath.Join(tmp, "..", "..", "..", "etc", "hosts")
	_, err := ValidatePathWithinRoots(escape)
	if err == nil {
		t.Error("expected error for ../.. escape")
	}
}

func TestValidate_RelativePath(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	// Create file in tmp.
	inside := filepath.Join(tmp, "rel.log")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}

	// Chdir to tmp so the relative path "rel.log" is meaningful.
	saved, _ := os.Getwd()
	defer os.Chdir(saved) // nolint: errcheck
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	got, err := ValidatePathWithinRoots("rel.log")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

func TestValidate_PathWithSpaces(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "has spaces")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "file name.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePathWithinRoots(f); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_UnicodePath(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	f := filepath.Join(tmp, "файл.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePathWithinRoots(f); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_PathIsRootItself(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	// The root directory itself should validate. Returned path is the
	// Abs form (see TestValidate_InsideRoot for the contract rationale).
	got, err := ValidatePathWithinRoots(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := filepath.Abs(tmp)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestValidate_MultipleRootsAnyMatch(t *testing.T) {
	t.Cleanup(Reset)
	r1 := t.TempDir()
	r2 := t.TempDir()
	if err := SetSearchRoots([]string{r1, r2}); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(r2, "in-second-root.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePathWithinRoots(f); err != nil {
		t.Errorf("expected path under r2 to validate, got %v", err)
	}
}

func TestValidate_PrefixAmbiguity(t *testing.T) {
	// Root /tmp/roota must NOT match path /tmp/rootaother/file.log.
	// This is the classic prefix bug — enforce the separator check.
	t.Cleanup(Reset)
	tmpParent := t.TempDir()
	rootA := filepath.Join(tmpParent, "roota")
	rootABOther := filepath.Join(tmpParent, "rootaother")
	if err := os.Mkdir(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootABOther, 0o755); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(rootABOther, "x.log")
	if err := os.WriteFile(otherFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetSearchRoots([]string{rootA}); err != nil {
		t.Fatal(err)
	}
	_, err := ValidatePathWithinRoots(otherFile)
	if err == nil {
		t.Errorf("path %q must NOT validate under root %q", otherFile, rootA)
	}
}

// Go-specific test (per plan §6.5.5 "new (Go-specific ENOENT case)"):
// ValidatePathWithinRoots must not fail on a nonexistent file; it's the
// CALLER's job to produce a nice 404-style error after the open fails.
func TestValidate_NonexistentPath_UsesAbsOnly(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	// This path doesn't exist; validation should still succeed because
	// Abs can resolve it.
	nonexistent := filepath.Join(tmp, "does-not-exist.log")
	got, err := ValidatePathWithinRoots(nonexistent)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

// Go-specific test: race-free behavior under concurrent SetSearchRoots + Validate.
//
// The `go test -race` instrumentation catches any unsynchronised access
// to the package globals during this window. Return values are ignored
// — this test cares about data-race absence, not value correctness
// (earlier tests cover value correctness).
func TestValidate_ConcurrentSetAndValidate(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	inside := filepath.Join(tmp, "r.log")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}

	tmp2 := t.TempDir()
	const nReaders = 20
	const iterations = 100

	// Reader goroutines each do bounded work; writer goroutine runs
	// until readers complete, then stops.
	var readers sync.WaitGroup
	stop := make(chan struct{})

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if toggle {
				_ = SetSearchRoots([]string{tmp, tmp2})
			} else {
				_ = SetSearchRoots([]string{tmp})
			}
			toggle = !toggle
		}
	}()

	for i := 0; i < nReaders; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < iterations; j++ {
				_, _ = ValidatePathWithinRoots(inside)
			}
		}()
	}
	readers.Wait()
	close(stop)
	<-writerDone

	// After the storm, restore a single-root sandbox so later tests
	// aren't surprised by leftover state (they each call Reset, but
	// we're being polite).
	_ = SetSearchRoots([]string{tmp})
}

func TestValidate_SymlinkToOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require admin on Windows CI")
	}
	t.Cleanup(Reset)

	// Layout:
	//   /tmp/tN/root/       (the sandboxed root)
	//   /tmp/tN/outside/    (outside the sandbox)
	//   /tmp/tN/root/link → /tmp/tN/outside/secret.txt
	//
	// A Validate call on /tmp/tN/root/link must FAIL because
	// EvalSymlinks resolves it to /tmp/tN/outside/secret.txt, which is
	// outside the root.
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := SetSearchRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	_, err := ValidatePathWithinRoots(link)
	if err == nil {
		t.Error("expected symlink escape to be rejected")
	}
}

func TestIsPathWithinRoots(t *testing.T) {
	t.Cleanup(Reset)
	tmp := t.TempDir()
	if err := SetSearchRoots([]string{tmp}); err != nil {
		t.Fatal(err)
	}
	if !IsPathWithinRoots(tmp) {
		t.Error("root should be within itself")
	}
	if IsPathWithinRoots("/etc/hosts") {
		t.Error("/etc/hosts should not be within root")
	}
}
