package signatures

import (
	"strings"
	"testing"
)

// A built-in whose id a refused user definition took is listed as
// replaced, with the definition's place, and is neither active nor
// disabled: a selection that names it answers with the load errors.
func TestLoad_RefusedOverrideMarksTheBuiltinReplaced(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "team.yml", `signatures:
  - id: blank-run
    name: Many blank lines
    group: structure
    description: Five blank lines in a row.
    run: {blank: true, min: 1}
`)
	cat := loadFiles(path)
	replaced := cat.Replaced()
	if len(replaced) != 1 || replaced[0].ID != "blank-run" || replaced[0].ReplacedBy == nil ||
		replaced[0].ReplacedBy.File != path || replaced[0].ReplacedBy.Line != 2 {
		t.Fatalf("replaced = %+v, want blank-run marked replaced by %s:2", replaced, path)
	}
	if _, ok := cat.Lookup("blank-run"); ok {
		t.Error("the replaced built-in is active")
	}
	if _, ok := cat.LookupDisabled("blank-run"); ok {
		t.Error("the replaced built-in is in the disabled set")
	}
	_, err := cat.Resolve("blank-run")
	if err == nil || !strings.Contains(err.Error(), "was not loaded") {
		t.Errorf("Resolve(blank-run) = %v, want \"was not loaded\" with the load errors", err)
	}
}

// disable of a built-in a user signature replaced is an error of the
// file, not a no-op: the user signature keeps running, and disable
// switches built-ins off.
func TestLoad_DisableOfAnOverriddenBuiltinIsAnError(t *testing.T) {
	dir := t.TempDir()
	first := writeSignatureFile(t, dir, "a-team.yml", `disable: [blank-run]
signatures:
  - id: blank-run
    name: Many blank lines
    group: structure
    description: Five blank lines in a row.
    run: {blank: true, min: 5}
  - id: invoice-failed
    name: Invoice failed
    group: severity
    description: An invoice that failed.
    regex: 'invoice failed'
`)
	later := writeSignatureFile(t, dir, "b-later.yml", "disable: [blank-run]\n")
	cat := loadFiles(first, later)
	blank := mustLookup(t, cat, "blank-run")
	if !blank.Overrides || blank.Run.Min != 5 {
		t.Errorf("the override does not run: %+v", blank)
	}
	mustLookup(t, cat, "invoice-failed")
	errs := cat.Errors()
	if len(errs) != 2 {
		t.Fatalf("errors %v, want one per file", errs)
	}
	for _, e := range errs {
		want := "disable: blank-run is replaced by " + first + ":3; disable switches built-ins off"
		if e.Message != want {
			t.Errorf("error %q, want %q", e.Message, want)
		}
	}
}

// Selection words are matched with their ASCII letters lowered, as the
// keyword gate lowers; a word with another letter matches nothing and
// is echoed as the user wrote it.
func TestResolve_LowersASCIIOnly(t *testing.T) {
	cat := Load(LoadOptions{Paths: []string{}})
	sel, err := cat.Resolve("TRACEBACK")
	if err != nil || len(sel) == 0 {
		t.Fatalf("Resolve(TRACEBACK) = %v, %v; want the traceback group", sel, err)
	}
	_, err = cat.Resolve("\u0130d")
	if err == nil || !strings.Contains(err.Error(), "\u0130d") {
		t.Errorf("Resolve(İd) = %v, want a refusal that echoes the word as written", err)
	}
}
