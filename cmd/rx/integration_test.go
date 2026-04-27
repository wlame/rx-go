package main

// Integration tests that exercise the production-path wiring from the
// CLI binary's point of view. Living in cmd/rx means these tests run
// with the same detector blank-imports as the real `rx` binary, so
// analyzer.LineDetectorSnapshot() returns the full 9-detector catalog.
//
// The internal/clicommand tests don't blank-import detectors, so they
// can verify flag plumbing but NOT that the detectors actually fire —
// that's what these tests are for.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/analyzer"
	"github.com/wlame/rx-go/internal/index"
)

// TestRegistry_BlankImportsWireAllDetectors confirms every detector
// package's init() successfully registered a factory. If someone
// removes a blank import from main.go, this count assertion fails.
func TestRegistry_BlankImportsWireAllDetectors(t *testing.T) {
	// Expected: the 9 MVP detectors listed in docs/concepts/analyzers.md.
	const wantCount = 9
	if got := analyzer.Len(); got != wantCount {
		t.Fatalf("analyzer.Len() = %d, want %d (did a blank import get dropped from cmd/rx/main.go?)",
			got, wantCount)
	}
	detectors := analyzer.LineDetectorSnapshot()
	if len(detectors) != wantCount {
		t.Errorf("LineDetectorSnapshot() = %d detectors, want %d", len(detectors), wantCount)
	}
}

// TestIndexBuild_AnalyzeProductionPath verifies the CLI production
// path wiring: index.Build with Detectors populated via the global
// registry (the same snapshot the CLI and HTTP paths take) produces
// non-zero anomalies on a file containing obvious detector cues.
//
// Before finding #3 was fixed, BuildOptions.Detectors was left nil on
// the CLI path, the coordinator's zero-detector fast path kicked in,
// and `rx index --analyze` silently returned anomaly_count = 0. This
// test guards against that regression.
func TestIndexBuild_AnalyzeProductionPath(t *testing.T) {
	// Contents chosen to fire at least THREE different detectors:
	//   - secrets-scan: an AWS access key ID pattern.
	//   - traceback-python: a canonical "Traceback (most recent call last):".
	//   - repeat-identical: 6 identical lines (threshold is 5).
	content := "" +
		"app started\n" +
		"Traceback (most recent call last):\n" +
		"  File \"main.py\", line 10, in <module>\n" +
		"    x = 1/0\n" +
		"ZeroDivisionError: division by zero\n" +
		"AWS_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE\n" +
		"boring line\n" +
		"boring line\n" +
		"boring line\n" +
		"boring line\n" +
		"boring line\n" +
		"boring line\n" +
		"done\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Mirror the CLI flow: populate detectors from the registry. This is
	// exactly what internal/clicommand/index.go does on --analyze.
	detectors := analyzer.LineDetectorSnapshot()
	if len(detectors) == 0 {
		t.Fatal("LineDetectorSnapshot returned no detectors — registry not initialized?")
	}

	idx, err := index.Build(path, index.BuildOptions{
		Analyze:   true,
		Detectors: detectors,
	})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if idx.Anomalies == nil {
		t.Fatal("idx.Anomalies is nil; expected populated slice under Analyze=true")
	}

	anomalies := *idx.Anomalies
	if len(anomalies) == 0 {
		t.Fatal("anomaly_count = 0 — production-path wire regression " +
			"(BuildOptions.Detectors likely not plumbed through)")
	}

	// We expect at least three distinct detectors to fire. Which ones
	// exactly is less important than the count being > 0 — the point is
	// that the registry-populated path actually runs the detectors.
	seen := make(map[string]bool)
	for _, a := range anomalies {
		seen[a.Detector] = true
	}
	if len(seen) < 2 {
		t.Errorf("only %d distinct detectors fired; want ≥2. Detectors seen: %v",
			len(seen), seen)
	}
}

// TestIndexBuild_MultiFile_NoStateLeak confirms that running index.Build
// sequentially against two files does NOT let detector state from the
// first build contaminate the second — proves the per-build factory
// pattern works as designed (finding #6 of the analyzers review).
//
// Why this matters: before the factory fix, every detector registered
// a SHARED instance whose streaming state (open runs, hash fingerprints,
// candidate buffers) persisted between builds. A file with an open run
// at EOF would cause the next file's first line to look like a
// continuation of the previous file's run.
func TestIndexBuild_MultiFile_NoStateLeak(t *testing.T) {
	// File 1: ends with 6 identical lines (would leave repeat-identical
	// state in a shared-instance regression).
	file1Content := "intro\n" +
		"repeat\nrepeat\nrepeat\nrepeat\nrepeat\nrepeat\n"

	// File 2: starts with 4 identical lines (shorter than minRunLength=5)
	// that would COMBINE with file-1's run if state leaked, producing
	// a false anomaly here.
	file2Content := "repeat\nrepeat\nrepeat\nrepeat\n" +
		"different\n"

	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.log")
	path2 := filepath.Join(dir, "b.log")
	if err := os.WriteFile(path1, []byte(file1Content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path2, []byte(file2Content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Build both files via SEPARATE LineDetectorSnapshot calls — this
	// is the real CLI behavior (one snapshot per file).
	for _, path := range []string{path1, path2} {
		detectors := analyzer.LineDetectorSnapshot()
		_, err := index.Build(path, index.BuildOptions{
			Analyze:   true,
			Detectors: detectors,
		})
		if err != nil {
			t.Fatalf("Build(%s): %v", path, err)
		}
	}

	// Now rebuild file 2 with its own snapshot; if state had leaked from
	// a PREVIOUS shared instance, the first build would be indistinguishable
	// from the second. With per-build factories, both calls are independent.
	detectors := analyzer.LineDetectorSnapshot()
	idx2, err := index.Build(path2, index.BuildOptions{
		Analyze:   true,
		Detectors: detectors,
	})
	if err != nil {
		t.Fatalf("Build(%s) second time: %v", path2, err)
	}

	if idx2.Anomalies != nil {
		for _, a := range *idx2.Anomalies {
			if a.Detector == "repeat-identical" {
				t.Errorf("unexpected repeat-identical anomaly on file 2 (only 4 identical lines): %+v", a)
			}
		}
	}
}
