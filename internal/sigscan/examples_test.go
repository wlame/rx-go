package sigscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// A signature whose `match` example finds nothing is refused
// at load, with its file, line and id; so is one whose `no_match`
// example finds something, and one whose declared keywords miss a line
// its regex matches. A signature whose examples pass loads.
func TestCheckExamples_RefusesAFailingExample(t *testing.T) {
	body := sigFile(
		regexSig("passes", `ERROR`, "examples:", "  match: ['x ERROR y']", "  no_match: ['all good']"),
		regexSig("finds-nothing", `ERROR`, "examples:", "  match: ['error in lower case']"),
		regexSig("finds-too-much", `ERROR`, "examples:", "  no_match: ['an ERROR here']"),
		regexSig("wrong-keyword", `timeout`, "keywords: [deadline]", "examples:", "  match: ['request timeout']"),
		kindSig("block-passes", "block", `start: '^BEGIN'`, `end: '^END'`)+
			"    examples:\n      match:\n        - |\n          BEGIN\n          body\n          END\n",
	)
	path := filepath.Join(t.TempDir(), "team.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{path}, CheckExamples: CheckExamples})
	refused := map[string]signatures.LoadError{}
	for _, e := range cat.Errors() {
		refused[e.ID] = e
	}
	for id, wantMessage := range map[string]string{
		"finds-nothing":  "examples: match example 1 gives no finding",
		"finds-too-much": "examples: no_match example 1 gives 1 finding(s), the first on line 1",
		"wrong-keyword":  "examples: match example 1 gives no finding",
	} {
		e, ok := refused[id]
		if !ok {
			t.Errorf("%s loaded; its example fails", id)
			continue
		}
		if e.File != path || e.Line == 0 || e.Message != wantMessage {
			t.Errorf("%s: refused as %+v, want file %s, a line, and %q", id, e, path, wantMessage)
		}
	}
	for _, id := range []string{"passes", "block-passes"} {
		if _, ok := cat.Lookup(id); !ok {
			t.Errorf("%s was refused: %v", id, refused[id])
		}
	}
}

// Every built-in signature passes its own examples.
func TestCheckExamples_BuiltinsPass(t *testing.T) {
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{}, CheckExamples: CheckExamples})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("built-in examples fail: %v", errs)
	}
	if !cat.ExamplesChecked() || len(cat.Signatures()) == 0 {
		t.Fatalf("examples checked %v, %d signatures", cat.ExamplesChecked(), len(cat.Signatures()))
	}
}

// A measure cannot be scanned by this engine: its examples refuse it,
// rather than pass unchecked.
func TestCheckExamples_MeasureIsRefused(t *testing.T) {
	body := "signatures:\n  - id: bursts\n    name: Bursts\n    group: time\n    description: Test.\n" +
		"    measure: {name: density}\n    examples:\n      match: ['x']\n"
	path := filepath.Join(t.TempDir(), "team.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{path}, CheckExamples: CheckExamples})
	if errs := cat.Errors(); len(errs) != 1 || !strings.Contains(errs[0].Message, "measure density") {
		t.Fatalf("errors %v, want the measure refused", errs)
	}
}
