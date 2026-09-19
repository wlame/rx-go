package clicommand

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A file whose name holds terminal control sequences cannot drive the
// terminal of a user who searches its directory: `rx trace` and
// `rx index` print the name with the controls written out.
func TestHumanOutputWritesOutControlsInFileNames(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "\x1b[31mred.log"), []byte("NEEDLE\n"), 0o600); err != nil {
		t.Skipf("this filesystem refuses the name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "\x1b]0;x\x07bin.dat"), []byte("NEEDLE\x00\n"), 0o600); err != nil {
		t.Skipf("this filesystem refuses the name: %v", err)
	}

	var traced bytes.Buffer
	cmd := NewTraceCommand(&traced)
	cmd.SetArgs([]string{"NEEDLE", dir, "--samples"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trace: %v", err)
	}
	var indexed bytes.Buffer
	zero := 0
	if err := runIndex(&indexed, indexParams{paths: []string{dir}, recursive: true, threshold: &zero}); err != nil {
		t.Fatalf("index: %v", err)
	}
	for name, out := range map[string]string{"trace": traced.String(), "index": indexed.String()} {
		if strings.ContainsAny(out, "\x1b\x07") {
			t.Errorf("%s output holds a raw control byte:\n%q", name, out)
		}
		if !strings.Contains(out, `\x1b[31mred.log`) {
			t.Errorf("%s output does not show the name written out:\n%q", name, out)
		}
	}
}
