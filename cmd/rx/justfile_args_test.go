package main

// The justfile's recipes run rx and go test with the words a developer
// types after the recipe name. A recipe that pastes a variadic
// parameter into its command line as {{args}} gives the shell the words
// joined by spaces: the shell splits them again at every space, so
// `just run trace 'a b' app.log` searches for `a` in the files `b` and
// `app.log`, and `just test -run='A|B'` runs `B` as a command. A recipe
// with the [positional-arguments] attribute receives its arguments as
// $1, $2, … instead, and "$@" passes each on as one word.

import (
	"encoding/json"
	"errors"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// justDump is the part of `just --dump --dump-format=json` this test
// reads: the recipes by name.
type justDump struct {
	Recipes map[string]justRecipe `json:"recipes"`
}

// justRecipe is one recipe of the dump.
//
// Attributes holds each attribute as JSON: a bare attribute such as
// positional-arguments is a string, one with arguments is an object.
// Body holds the recipe's lines; each line is a list of fragments, and a
// fragment is either literal text (a JSON string) or an interpolation
// (a JSON array holding the expression between {{ and }}).
type justRecipe struct {
	Attributes []json.RawMessage `json:"attributes"`
	Parameters []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"parameters"`
	Body [][]json.RawMessage `json:"body"`
}

// variadicParameterKinds are the dump's kinds for a parameter that takes
// any number of words: `*args` (zero or more) and `+args` (one or more).
var variadicParameterKinds = []string{"star", "plus"}

// hasAttribute reports whether the recipe carries the bare attribute
// name, such as positional-arguments.
func (r justRecipe) hasAttribute(name string) bool {
	for _, raw := range r.Attributes {
		var bare string
		if json.Unmarshal(raw, &bare) == nil && bare == name {
			return true
		}
	}
	return false
}

// interpolates reports whether a {{…}} expression of the recipe's body
// reads the variable or parameter name.
func (r justRecipe) interpolates(name string) bool {
	for _, line := range r.Body {
		for _, raw := range line {
			var fragment any
			if json.Unmarshal(raw, &fragment) != nil {
				continue
			}
			if _, literal := fragment.(string); literal {
				continue
			}
			if readsVariable(fragment, name) {
				return true
			}
		}
	}
	return false
}

// readsVariable reports whether the expression tree node, as
// encoding/json decodes it into `any`, holds the node ["variable", name]
// at any depth. A concatenation or a function call nests its operands
// as arrays, so the walk descends into every array.
func readsVariable(node any, name string) bool {
	items, isArray := node.([]any)
	if !isArray {
		return false
	}
	if len(items) == 2 && items[0] == "variable" && items[1] == name {
		return true
	}
	for _, item := range items {
		if readsVariable(item, name) {
			return true
		}
	}
	return false
}

// Every justfile recipe with a variadic parameter receives its
// arguments as positional arguments and never pastes the parameter into
// its command line, so each word a developer types reaches go test or
// rx as one word.
func TestJustfile_VariadicRecipesPassEachArgumentAsOneWord(t *testing.T) {
	justPath, err := exec.LookPath("just")
	if err != nil {
		t.Skip("just is not on PATH; the justfile's recipes cannot be read")
	}
	justfile := filepath.Join("..", "..", "justfile")
	out, err := exec.Command(justPath, "--justfile="+justfile, "--dump", "--dump-format=json").Output()
	if err != nil {
		// Output keeps the command's stderr in the *exec.ExitError it
		// returns for a non-zero exit; it says what just could not read.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("just --dump: %v: %s", err, exitErr.Stderr)
		}
		t.Fatalf("just --dump: %v", err)
	}
	var dump justDump
	if err := json.Unmarshal(out, &dump); err != nil {
		t.Fatalf("decode the dump of %s: %v", justfile, err)
	}

	checked := 0
	// Sorted names give the failures in the same order on every run.
	for _, name := range slices.Sorted(maps.Keys(dump.Recipes)) {
		recipe := dump.Recipes[name]
		for _, param := range recipe.Parameters {
			if !slices.Contains(variadicParameterKinds, param.Kind) {
				continue
			}
			checked++
			if !recipe.hasAttribute("positional-arguments") {
				t.Errorf("recipe %s takes the variadic %s without [positional-arguments]; its arguments are split at spaces", name, param.Name)
			}
			if recipe.interpolates(param.Name) {
				t.Errorf("recipe %s pastes {{%s}} into its command line; pass \"$@\" instead", name, param.Name)
			}
		}
	}
	// The justfile has had variadic recipes (test, run, bench) from the
	// start: none found means the dump's format changed under the test.
	if checked == 0 {
		t.Fatal("the dump shows no recipe with a variadic parameter; did the format of just --dump change?")
	}
}
