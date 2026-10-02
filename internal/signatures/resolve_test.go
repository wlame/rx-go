package signatures

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// selectionFile adds user signatures to the built-ins for the selection
// tests: tags, an opt-in signature, an id that is also a group's name,
// and a signature the loader refuses.
const selectionFile = `signatures:
  - id: my-check
    name: My check
    group: severity
    tags: [postgres]
    description: D.
    regex: 'mycheck'
  - id: slow-query
    name: Slow query
    group: sql
    tags: [postgres, perf]
    description: D.
    regex: 'duration: \d+ ms'
  - id: email
    name: Email
    group: pii
    description: D.
    default: false
    regex: '@example\.com'
  - id: structure
    name: Structure
    group: severity
    description: D.
    regex: 'structure'
  - id: broken
    name: Broken
    group: severity
    description: D.
    regex: '(oops'
`

// selectionCatalogue loads the built-ins and selectionFile.
func selectionCatalogue(t *testing.T) (*Catalogue, string) {
	t.Helper()
	path := writeSignatureFile(t, t.TempDir(), "sel.yml", selectionFile+disableBuiltinsExcept(t, testBuiltins...))
	return loadFiles(path), path
}

// ids lists the ids of signatures.
func ids(sigs []*Signature) []string {
	out := make([]string, len(sigs))
	for i, s := range sigs {
		out[i] = s.ID
	}
	return out
}

// A selection is resolved word by word, left to right, into signatures
// in catalogue order: ids first, then groups, then tags; all, default,
// group:, tag: and removals.
func TestResolve(t *testing.T) {
	cat, _ := selectionCatalogue(t)
	everyDefault := []string{"python-traceback", "aws-access-key", "slow-query", "my-check", "structure", "blank-run"}
	cases := []struct {
		expr string
		want []string
	}{
		{"traceback,sql,my-check", []string{"python-traceback", "slow-query", "my-check"}},
		{"all,-pii", everyDefault},
		{"all", []string{"python-traceback", "aws-access-key", "email", "slow-query", "my-check", "structure", "blank-run"}},
		{"tag:postgres", []string{"slow-query", "my-check"}},
		{"postgres", []string{"slow-query", "my-check"}},
		{"structure", []string{"structure"}},
		{"group:structure", []string{"blank-run"}},
		{"", everyDefault},
		{" , ", everyDefault},
		{"default", everyDefault},
		{"default,email", []string{"python-traceback", "aws-access-key", "email", "slow-query", "my-check", "structure", "blank-run"}},
		{"TRACEBACK, Tag:Perf", []string{"python-traceback", "slow-query"}},
		{"sql,-slow-query,my-check", []string{"my-check"}},
		{"-sql,sql", []string{"slow-query"}},
	}
	for _, tc := range cases {
		got, err := cat.Resolve(tc.expr)
		if err != nil {
			t.Errorf("%q: %v", tc.expr, err)
			continue
		}
		if !slices.Equal(ids(got), tc.want) {
			t.Errorf("%q gives %v, want %v", tc.expr, ids(got), tc.want)
		}
	}
}

// An unknown word, an unknown group or tag, an empty result and a lone
// dash are selection errors; an unknown word lists close names.
func TestResolve_Errors(t *testing.T) {
	cat, _ := selectionCatalogue(t)
	cases := []struct {
		expr        string
		message     string
		suggestions []string
	}{
		{"tracebak", `unknown signature, group or tag "tracebak"`, []string{"traceback"}},
		{"pyhton-traceback", "unknown", []string{"python-traceback"}},
		{"group:sqll", `unknown group "sqll"`, []string{"sql"}},
		{"tag:postgre", `unknown tag "postgre"`, []string{"postgres"}},
		{"-pii", "selects no signature", nil},
		{"all,-all", "selects no signature", nil},
		{"sql,-", "empty word", nil},
		{"group:", "empty word", nil},
		{strings.Repeat("sql,", 1001), "at most 1000", nil},
	}
	for _, tc := range cases {
		_, err := cat.Resolve(tc.expr)
		var sel *SelectionError
		if !errors.As(err, &sel) {
			t.Errorf("%q: error %v, want a *SelectionError", tc.expr, err)
			continue
		}
		if !strings.Contains(sel.Error(), tc.message) {
			t.Errorf("%q: %q, want %q in it", tc.expr, sel.Error(), tc.message)
		}
		for _, name := range tc.suggestions {
			if !slices.Contains(sel.Suggestions, name) || !strings.Contains(sel.Error(), name) {
				t.Errorf("%q: suggestions %v, want %q among them", tc.expr, sel.Suggestions, name)
			}
		}
		if len(sel.Suggestions) > 5 {
			t.Errorf("%q: %d suggestions", tc.expr, len(sel.Suggestions))
		}
	}
}

// A word that names a refused signature repeats the load error.
func TestResolve_RefusedSignature(t *testing.T) {
	cat, path := selectionCatalogue(t)
	_, err := cat.Resolve("my-check,broken")
	var sel *SelectionError
	if !errors.As(err, &sel) {
		t.Fatalf("error %v", err)
	}
	if len(sel.LoadErrors) != 1 || sel.LoadErrors[0].ID != "broken" {
		t.Fatalf("load errors %v", sel.LoadErrors)
	}
	if !strings.Contains(sel.Error(), path+":29") || !strings.Contains(sel.Error(), "not loaded") {
		t.Errorf("message %q", sel.Error())
	}
}

// A disabled built-in named in a selection is an error that says where
// it was disabled; a removal of it is no error, and groups leave it out.
func TestResolve_DisabledBuiltin(t *testing.T) {
	path := writeSignatureFile(t, t.TempDir(), "d.yml", disableBuiltinsExcept(t, "python-traceback", "aws-access-key"))
	cat := loadFiles(path)
	_, err := cat.Resolve("blank-run")
	var sel *SelectionError
	if !errors.As(err, &sel) || !strings.Contains(sel.Error(), "disabled by "+path+":1") {
		t.Fatalf("error %v", err)
	}
	got, err := cat.Resolve("all,-blank-run")
	if err != nil || !slices.Equal(ids(got), []string{"python-traceback", "aws-access-key"}) {
		t.Errorf("all,-blank-run: %v %v", ids(got), err)
	}
	if _, err := cat.Resolve("structure"); err == nil {
		t.Error("a group whose only signature is disabled selects something")
	}
}

// CloseNames orders names by edit distance and keeps at most five.
func TestCloseNames(t *testing.T) {
	names := []string{"traceback", "python-traceback", "sql", "structure", "time", "tag", "trace", "track", "tracer", "tracing"}
	got := CloseNames("trace", names)
	if len(got) > 5 || got[0] != "trace" {
		t.Errorf("%v", got)
	}
	if got := CloseNames("zzzzzz", names); len(got) != 0 {
		t.Errorf("nothing close: %v", got)
	}
	if got := CloseNames("python", names); !slices.Contains(got, "python-traceback") {
		t.Errorf("a name that holds the word: %v", got)
	}
}

// FuzzResolve: any selection text gives either signatures or a
// *SelectionError, never a panic and never an empty success.
func FuzzResolve(f *testing.F) {
	for _, seed := range []string{"traceback,sql,my-check", "all,-pii", "tag:postgres", "group:x", "", "-", ",,", "tag:", "a,-b,c", "\x00", "ALL"} {
		f.Add(seed)
	}
	cat := Load(LoadOptions{Paths: []string{}})
	f.Fuzz(func(t *testing.T, expr string) {
		got, err := cat.Resolve(expr)
		if err != nil {
			var sel *SelectionError
			if !errors.As(err, &sel) {
				t.Fatalf("%q: error %T %v", expr, err, err)
			}
			return
		}
		if len(got) == 0 {
			t.Fatalf("%q selects nothing without an error", expr)
		}
	})
}
