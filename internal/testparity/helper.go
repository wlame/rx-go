// Package testparity provides helpers to cross-check rx-go output
// against the rx-python reference implementation. Used by high-level
// parity tests that run both binaries over identical inputs and
// assert their outputs match.
//
// The helpers live in a dedicated package (not under the package being
// tested) so that multiple test packages can share them without import
// cycles. They are safe to import from any _test.go file.
//
// Typical use:
//
//	func TestParity_Trace(t *testing.T) {
//	    fixture := testparity.FixturePath("medium.log")
//	    pyOut, err := testparity.RunPythonRx(t, "trace", "error", fixture, "--json")
//	    // ...
//	    goOut, err := testparity.RunGoRx(t, "trace", "error", fixture, "--json")
//	    testparity.AssertJSONParity(t, pyOut, goOut, []string{"time", "request_id"})
//	}
package testparity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// FixturesDir returns the absolute path to rx-go/testdata/fixtures. Uses
// runtime.Caller to locate itself, so callers don't need to know their
// CWD (Go tests run from the package being tested).
func FixturesDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "fixtures")
}

// FixturePath returns the absolute path to a named fixture. Callers pass
// the basename (e.g. "medium.log"); the helper rejoins against the
// package-global fixtures directory.
func FixturePath(name string) string {
	return filepath.Join(FixturesDir(), name)
}

// GoBinaryPath returns the path where the test-built rx-go binary lives.
// Tests that need a real static binary should compile it via
// BuildGoBinary, which writes to this path.
func GoBinaryPath() string {
	return filepath.Join(os.TempDir(), "rx-go-test-bin")
}

// BuildGoBinary compiles cmd/rx with the same ldflags used in CI and
// writes it to GoBinaryPath. Caller is responsible for calling this
// from TestMain or a setup helper.
//
// Thread-safety: NOT goroutine-safe. Call from TestMain or serially.
func BuildGoBinary(t *testing.T) string {
	t.Helper()
	path := GoBinaryPath()
	if info, err := os.Stat(path); err == nil {
		// Already built recently? Skip unless stale (>5 min).
		if time.Since(info.ModTime()) < 5*time.Minute {
			return path
		}
	}
	// Walk up from this file's dir to find go.mod.
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod")
		}
		dir = parent
	}

	cmd := exec.Command("go", "build",
		"-ldflags=-s -w -X main.appVersion=2.2.1-go",
		"-o", path,
		"./cmd/rx")
	cmd.Dir = dir
	// Respect GOSUMDB=off from outer shell in constrained CI environments.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build rx-go: %v\nstderr:\n%s", err, stderr.String())
	}
	return path
}

// RunGoRx executes the pre-built rx-go binary with the given args and
// returns stdout. stderr is captured into t.Log on non-zero exit.
func RunGoRx(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	path := BuildGoBinary(t)
	cmd := exec.Command(path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("rx-go stderr: %s", stderr.String())
	}
	return stdout.Bytes(), err
}

// RunPythonRx executes rx-python's CLI through its virtual environment.
// Uses RX_PYTHON_PATH env var to locate the project if set; falls back
// to ../rx-python relative to the rx-go module root.
//
// If the python venv isn't present, the returned error is a specific
// one the caller can check with errors.Is / IsPythonUnavailable to
// decide whether to SkipIf.
func RunPythonRx(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	pyRoot := os.Getenv("RX_PYTHON_PATH")
	if pyRoot == "" {
		// Walk up to find the monorepo root.
		_, thisFile, _, _ := runtime.Caller(0)
		root := filepath.Dir(thisFile)
		for i := 0; i < 5; i++ {
			candidate := filepath.Join(root, "..", "rx-python")
			if _, err := os.Stat(candidate); err == nil {
				pyRoot = candidate
				break
			}
			root = filepath.Dir(root)
		}
	}
	if pyRoot == "" {
		return nil, ErrPythonUnavailable
	}
	venvPython := filepath.Join(pyRoot, ".venv", "bin", "python")
	// #nosec G703 -- path is developer-controlled (RX_PYTHON_PATH) or
	// derived from the rx-python checkout beside rx-go. Not a user input.
	if _, err := os.Stat(venvPython); err != nil {
		return nil, ErrPythonUnavailable
	}
	fullArgs := append([]string{"-m", "rx.cli.main"}, args...)
	// #nosec G702 -- same rationale: venvPython is developer-controlled.
	cmd := exec.Command(venvPython, fullArgs...)
	cmd.Dir = pyRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("rx-python stderr: %s", stderr.String())
	}
	return stdout.Bytes(), err
}

// ErrPythonUnavailable is returned by RunPythonRx when the venv can't
// be located. Tests should t.Skip on this so CI environments without
// Python don't fail.
var ErrPythonUnavailable = fmt.Errorf("rx-python venv not found; set RX_PYTHON_PATH or place rx-python beside rx-go")

// IsPythonUnavailable returns true iff err is (or wraps) the python-
// unavailable sentinel.
func IsPythonUnavailable(err error) bool {
	return err == ErrPythonUnavailable
}

// AssertJSONParity compares two JSON outputs field-by-field, ignoring
// the field names listed in `ignoreFields`. This is the workhorse of
// parity testing — most parity mismatches are real structural drifts,
// but a handful of fields (timing, UUIDs, absolute paths) are expected
// to differ run-to-run and need explicit ignoring.
func AssertJSONParity(t *testing.T, pyOutput, goOutput []byte, ignoreFields []string) {
	t.Helper()
	py, go1, err := parseBoth(pyOutput, goOutput)
	if err != nil {
		t.Fatalf("parse JSON: %v\npy=%s\ngo=%s", err, pyOutput, goOutput)
	}
	ignoreMap := map[string]bool{}
	for _, f := range ignoreFields {
		ignoreMap[f] = true
	}
	if diff := diffJSON("", py, go1, ignoreMap); diff != "" {
		t.Errorf("JSON parity mismatch:\n%s", diff)
	}
}

// parseBoth is a helper to decode two JSON byte slices into `any`.
func parseBoth(a, b []byte) (any, any, error) {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return nil, nil, fmt.Errorf("a: %w", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return nil, nil, fmt.Errorf("b: %w", err)
	}
	return av, bv, nil
}

// diffJSON walks two JSON values recursively and returns a human-readable
// string describing the first mismatch, or "" if the structures match.
//
// `path` accumulates the field/index trail for error reporting.
// `ignore` is a set of leaf field NAMES (not paths) to skip.
func diffJSON(path string, a, b any, ignore map[string]bool) string {
	// Both nil.
	if a == nil && b == nil {
		return ""
	}
	// Type mismatch.
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: type differs (py=map, go=%T)", path, b)
		}
		return diffMap(path, av, bv, ignore)
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("%s: type differs (py=array, go=%T)", path, b)
		}
		return diffArray(path, av, bv, ignore)
	default:
		if fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) {
			return fmt.Sprintf("%s: py=%v, go=%v", path, a, b)
		}
		return ""
	}
}

// diffMap compares the union of keys present in either map.
func diffMap(path string, a, b map[string]any, ignore map[string]bool) string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	for k := range seen {
		if ignore[k] {
			continue
		}
		subPath := path + "." + k
		av, aOk := a[k]
		bv, bOk := b[k]
		if !aOk {
			return fmt.Sprintf("%s: only present in go side (value=%v)", subPath, bv)
		}
		if !bOk {
			return fmt.Sprintf("%s: only present in py side (value=%v)", subPath, av)
		}
		if d := diffJSON(subPath, av, bv, ignore); d != "" {
			return d
		}
	}
	return ""
}

// diffArray compares two JSON arrays element-wise. Length mismatch
// surfaces before individual diffs.
func diffArray(path string, a, b []any, ignore map[string]bool) string {
	if len(a) != len(b) {
		return fmt.Sprintf("%s: length differs (py=%d, go=%d)", path, len(a), len(b))
	}
	for i := range a {
		subPath := fmt.Sprintf("%s[%d]", path, i)
		if d := diffJSON(subPath, a[i], b[i], ignore); d != "" {
			return d
		}
	}
	return ""
}
