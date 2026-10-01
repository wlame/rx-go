package main

// `rx signatures` reads signature files; no other command of this build
// does. These tests run the binary, since its exit codes only exist
// once the error reaches os.Exit.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signatureEnv points the binary at dir for its signature files.
func signatureEnv(dir string) []string {
	return []string{"RX_SIGNATURES_PATH=" + dir}
}

// writeSignatureDir writes one signature file into a new directory.
func writeSignatureDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rx signatures check exits 0 on clean files, 1 on an error, and 2 on a
// usage error; the other subcommands exit 2 for a bad command line.
func TestSignatures_ExitCodes(t *testing.T) {
	clean := writeSignatureDir(t, `signatures:
  - id: invoice-failed
    name: Invoice failed
    group: severity
    description: An invoice that failed.
    regex: '\binvoice \d+ failed\b'
`)
	broken := writeSignatureDir(t, "signatures:\n  - id: broken-one\n    name: B\n    group: severity\n    description: B.\n    regex: '(oops'\n")
	cases := []struct {
		label string
		dir   string
		args  []string
		code  int
	}{
		{"check of clean files", clean, []string{"signatures", "check"}, 0},
		{"check of a broken file", broken, []string{"signatures", "check"}, 1},
		{"check with an unknown flag", clean, []string{"signatures", "check", "--bogus"}, 2},
		{"an unknown subcommand", clean, []string{"signatures", "nope"}, 2},
		{"show without an id", clean, []string{"signatures", "show"}, 2},
		{"show of an unknown id", clean, []string{"signatures", "show", "invoice-faild"}, 2},
		{"show of a refused signature", broken, []string{"signatures", "show", "broken-one"}, 2},
		{"the list", clean, []string{"signatures"}, 0},
		{"the template", clean, []string{"signatures", "template"}, 0},
	}
	for _, tc := range cases {
		code, stdout, stderr := runRxEnv(t, signatureEnv(tc.dir), tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d\nstdout: %s\nstderr: %s", tc.label, code, tc.code, stdout, stderr)
		}
	}
}

// A command that does not use signatures never reads the signature
// directory: rx samples answers with the directory unreadable, while
// rx signatures, given the same directory, reports it.
func TestSamples_DoesNotReadTheSignatureDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	sigDir := writeSignatureDir(t, "signatures: [\n")
	if err := os.Chmod(sigDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sigDir, 0o700) })
	_, logPath := writeLog(t)

	code, stdout, stderr := runRxEnv(t, signatureEnv(sigDir), "samples", logPath, "--lines=2")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "error happened here") {
		t.Errorf("rx samples: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	_, listed, _ := runRxEnv(t, signatureEnv(sigDir), "signatures")
	if !strings.Contains(listed, "cannot read the directory") {
		t.Errorf("rx signatures does not report the unreadable directory:\n%s", listed)
	}
}

// rx signatures check runs every signature's examples with the scan
// engine: the built-ins pass theirs, and a user signature whose match
// example finds nothing is refused with its file, line and id (exit 1).
func TestSignatures_CheckRunsTheExamples(t *testing.T) {
	dir := writeSignatureDir(t, `signatures:
  - id: invoice-failed
    name: Invoice failed
    group: severity
    description: An invoice that failed.
    regex: '\binvoice \d+ failed\b'
    examples:
      match: ['invoice 7 was paid']
`)
	code, stdout, stderr := runRxEnv(t, signatureEnv(dir), "signatures", "check")
	want := filepath.Join(dir, "team.yml") + ":7: invoice-failed: error: examples: match example 1 gives no finding"
	if code != 1 || !strings.Contains(stdout, want) || !strings.Contains(stdout, "examples: checked") {
		t.Errorf("exit %d, want 1 and the line %q and \"examples: checked\"\nstdout: %s\nstderr: %s", code, want, stdout, stderr)
	}
	code, stdout, _ = runRxEnv(t, signatureEnv(t.TempDir()), "signatures", "show", "python-traceback")
	if code != 0 || !strings.Contains(stdout, "# examples: 1 match, 1 no_match, all pass") {
		t.Errorf("rx signatures show of a built-in: exit %d\n%s", code, stdout)
	}
}
