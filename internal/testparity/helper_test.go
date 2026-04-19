package testparity

import (
	"os"
	"testing"
)

// TestFixtures_Created verifies EnsureFixtures produces the basic files.
func TestFixtures_Created(t *testing.T) {
	EnsureFixtures(t)
	for _, name := range []string{FixtureTiny, FixtureMedium, FixtureBinary} {
		info, err := os.Stat(FixturePath(name))
		if err != nil {
			t.Errorf("fixture %s missing: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("fixture %s is empty", name)
		}
	}
}

// TestCompressedFixtures verifies EnsureCompressedFixtures creates all
// 4 variants of a source fixture.
func TestCompressedFixtures(t *testing.T) {
	EnsureFixtures(t)
	src := FixturePath(FixtureTiny)
	got := EnsureCompressedFixtures(t, src)

	// We expect entries for gz, xz, zst, and possibly bz2 (skipped on
	// hosts without bzip2).
	wantFormats := []string{"gz", "xz", "zst"}
	for _, fmtName := range wantFormats {
		path, ok := got[fmtName]
		if !ok {
			t.Errorf("missing %s variant in result map", fmtName)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s variant not on disk: %v", fmtName, err)
			continue
		}
		// Tiny is ~220 bytes. Compressed should typically be < 300 bytes,
		// but we only assert non-empty to stay robust to library updates.
		if info.Size() == 0 {
			t.Errorf("%s variant is empty", fmtName)
		}
	}
}

// TestAssertJSONParity_BasicMatch verifies the differ returns "" for
// identical JSON payloads.
func TestAssertJSONParity_BasicMatch(t *testing.T) {
	a := []byte(`{"ok":true,"count":5}`)
	AssertJSONParity(t, a, a, nil)
}

// TestAssertJSONParity_IgnoresFields verifies the ignoreFields list
// suppresses differences on named keys.
func TestAssertJSONParity_IgnoresFields(t *testing.T) {
	a := []byte(`{"ok":true,"time":1.5}`)
	b := []byte(`{"ok":true,"time":9.9}`)
	AssertJSONParity(t, a, b, []string{"time"})
}

// TestDiffJSON_StructuralDifferences catches the common failure modes.
func TestDiffJSON_StructuralDifferences(t *testing.T) {
	cases := []struct {
		name     string
		a, b     []byte
		wantDiff bool
	}{
		{"scalar_match", []byte(`5`), []byte(`5`), false},
		{"scalar_differ", []byte(`5`), []byte(`6`), true},
		{"array_length", []byte(`[1,2]`), []byte(`[1,2,3]`), true},
		{"map_extra_key", []byte(`{"a":1}`), []byte(`{"a":1,"b":2}`), true},
		{"map_missing_key", []byte(`{"a":1,"b":2}`), []byte(`{"a":1}`), true},
		{"nested_match", []byte(`{"a":[{"b":1}]}`), []byte(`{"a":[{"b":1}]}`), false},
		{"nested_differ", []byte(`{"a":[{"b":1}]}`), []byte(`{"a":[{"b":2}]}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			av, bv, err := parseBoth(tc.a, tc.b)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			d := diffJSON("", av, bv, nil)
			got := d != ""
			if got != tc.wantDiff {
				t.Errorf("diff=%v (%q), want diff=%v", got, d, tc.wantDiff)
			}
		})
	}
}

// TestPythonRunner_SkipsIfMissing shows that an absent rx-python tree
// yields the sentinel, letting tests skip cleanly.
func TestPythonRunner_SkipsIfMissing(t *testing.T) {
	t.Setenv("RX_PYTHON_PATH", "/definitely/not/a/path")
	_, err := RunPythonRx(t, "--version")
	if !IsPythonUnavailable(err) {
		t.Errorf("expected ErrPythonUnavailable, got %v", err)
	}
}
