package clicommand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A built-in a refused user definition replaced keeps its row, marked
// with the definition's place, and the group's header counts the rows
// under it: the active signatures, then how many are disabled or
// replaced.
func TestSignaturesList_MarksAReplacedBuiltinAndCountsTheRows(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "team.yml")
	if err := os.WriteFile(file, []byte(`disable: [long-line]
signatures:
  - id: blank-run
    name: Many blank lines
    group: structure
    description: Five blank lines in a row.
    run: {blank: true, min: 1}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RX_SIGNATURES_PATH", dir)
	var buf strings.Builder
	cmd := NewSignaturesCommand(&buf, nil)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rx signatures: %v", err)
	}
	out := buf.String()
	folded := []string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Join(strings.Fields(line), " ")
		if strings.HasPrefix(line, "  ") {
			f = "  " + f
		}
		folded = append(folded, f)
	}
	joined := strings.Join(folded, "\n")
	for _, want := range []string{
		"structure 7 signatures, 1 disabled, 1 replaced",
		"  long-line low measure built-in disabled by " + file + ":1",
		"  blank-run info run built-in replaced by a refused definition (" + file + ":3)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
