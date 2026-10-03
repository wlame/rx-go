package cachefill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
)

// testSignatures are the signatures of the scan tests: err-line and
// py-trace in the default set, opt-sql opt-in. Every built-in is
// disabled, so the default set is exactly err-line and py-trace.
const testSignatures = `
  - id: err-line
    name: Error line
    group: severity
    description: A line that says ERROR.
    regex: 'ERROR'
  - id: py-trace
    name: Python traceback
    group: traceback
    description: A traceback with its frames.
    block:
      start: '^Traceback \(most recent call last\):'
      continue: '^\s'
      end: '^[A-Za-z]+Error: '
  - id: opt-sql
    name: SQL select
    group: sql
    description: A SELECT statement.
    default: false
    regex: '(?i)select\s'
`

// appLog is an invented log: err-line on lines 2 and 6, py-trace on
// lines 3-5.
const appLog = "2026-01-15 10:00:00 INFO start\n" +
	"2026-01-15 10:00:01 ERROR disk full\n" +
	"Traceback (most recent call last):\n" +
	"  File \"app.py\", line 3, in <module>\n" +
	"ValueError: bad input\n" +
	"2026-01-15 10:00:05 ERROR again\n"

// loadTestCatalogue loads testSignatures with every built-in disabled.
func loadTestCatalogue(t *testing.T) *signatures.Catalogue {
	t.Helper()
	var builtins []string
	for _, sig := range signatures.Load(signatures.LoadOptions{Paths: []string{}}).Signatures() {
		builtins = append(builtins, sig.ID)
	}
	body := "disable: [" + strings.Join(builtins, ", ") + "]\nsignatures:\n" + testSignatures
	path := filepath.Join(t.TempDir(), "team.yml")
	writeFile(t, path, body)
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{path}, CheckExamples: sigscan.CheckExamples})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return cat
}

// scanSettings are the settings of a scan of sel over a fresh findings
// cache directory.
func scanSettings(t *testing.T, cat *signatures.Catalogue, selection string) ScanSettings {
	t.Helper()
	sel, err := cat.Resolve(selection)
	if err != nil {
		t.Fatal(err)
	}
	return ScanSettings{
		Catalogue: cat, Selection: sel,
		Options: findings.ScanOptions{Dir: t.TempDir(), LogZone: "UTC", Scan: sigscan.Options{Workers: 2}},
	}
}

// writeFile writes text to path, making its directory first.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pin pins path, failing the test on an error.
func pin(t *testing.T, path string) paths.Pinned {
	t.Helper()
	pinned, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}

// walked is the entry a walk would give for the file at path.
func walked(t *testing.T, path string) Entry {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return Entry{Path: path}
}

// folder builds a directory of invented files: app.log, sub/b.log,
// sub/deeper/c.log, a binary blob.bin and a hidden .secret.log, and
// returns its path.
func folder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.log"), appLog)
	writeFile(t, filepath.Join(dir, "sub", "b.log"), "2026-01-15 11:00:00 ERROR elsewhere\n")
	writeFile(t, filepath.Join(dir, "sub", "deeper", "c.log"), "2026-01-15 12:00:00 INFO quiet\n")
	writeFile(t, filepath.Join(dir, "blob.bin"), "\x7fELF\x00\x00\x00\x01")
	writeFile(t, filepath.Join(dir, ".secret.log"), "ERROR hidden\n")
	return dir
}
