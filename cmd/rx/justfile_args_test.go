package main

// The justfile's recipes run rx and go test with the words a developer
// types after the recipe name. A recipe that pastes a variadic
// parameter into its command line as {{args}} gives the shell the words
// joined by spaces: the shell splits them again at every space, so
// `just run trace 'a b' app.log` searches for `a` in the files `b` and
// `app.log`, and `just test -run='A|B'` runs `B` as a command. A recipe
// with the [positional-arguments] attribute receives its arguments as
// $1, $2, … instead, and "$@" passes each on as one word.
//
// The same holds for every value that does not come from the justfile's
// own text. just pastes a {{…}} expression into the shell line before
// the shell reads it, so the shell reads the value as code: a quote in
// it ends the quoted string around it, and a `;`, `$(` or backtick after
// that starts a command. The version a build stamps comes from
// `git describe`, which prints a tag name, and a tag name may hold all
// of those characters.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// justDump is the part of `just --dump --dump-format=json` these tests
// read: the top-level assignments and the recipes, by name.
type justDump struct {
	Assignments map[string]justAssignment `json:"assignments"`
	Recipes     map[string]justRecipe     `json:"recipes"`
}

// justAssignment is one top-level `name := value` of the dump.
//
// Value is a JSON string for a string literal, and an array for a value
// just computes when it runs: ["evaluate", "…"] for a backtick,
// ["call", …] for a function such as env_var, ["concatenate", …] for a +.
type justAssignment struct {
	Value json.RawMessage `json:"value"`
}

// isLiteral reports whether the assignment's value is a string literal
// written in the justfile, so it is the same on every machine and run.
func (a justAssignment) isLiteral() bool {
	var literal string
	return json.Unmarshal(a.Value, &literal) == nil
}

// justParameter is one parameter of a recipe. Kind is "singular" for a
// parameter that takes one word, "star" for `*args` (zero or more
// words) and "plus" for `+args` (one or more).
type justParameter struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// justRecipe is one recipe of the dump.
//
// Attributes holds each attribute as JSON: a bare attribute such as
// positional-arguments is a string, one with arguments is an object.
// Body holds the recipe's lines; each line is a list of fragments, and a
// fragment is either literal text (a JSON string) or an interpolation
// (a JSON array holding the expression between {{ and }}).
type justRecipe struct {
	Attributes []json.RawMessage   `json:"attributes"`
	Parameters []justParameter     `json:"parameters"`
	Body       [][]json.RawMessage `json:"body"`
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

// hasParameter reports whether the recipe declares a parameter called
// name. Inside the recipe, a parameter hides a top-level variable of the
// same name.
func (r justRecipe) hasParameter(name string) bool {
	return slices.ContainsFunc(r.Parameters, func(p justParameter) bool { return p.Name == name })
}

// interpolatedVariables returns the names of the variables and
// parameters that the {{…}} expressions of the recipe's body read,
// sorted, each once.
func (r justRecipe) interpolatedVariables() []string {
	var names []string
	for _, line := range r.Body {
		for _, raw := range line {
			var fragment any
			if json.Unmarshal(raw, &fragment) != nil {
				continue
			}
			if _, literal := fragment.(string); literal {
				continue
			}
			names = appendVariablesRead(names, fragment)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// interpolates reports whether a {{…}} expression of the recipe's body
// reads the variable or parameter name.
func (r justRecipe) interpolates(name string) bool {
	return slices.Contains(r.interpolatedVariables(), name)
}

// appendVariablesRead appends to names the name of every node
// ["variable", name] in the expression tree node, as encoding/json
// decodes it into `any`, at any depth. A concatenation or a function
// call nests its operands as arrays, so the walk descends into every
// array.
func appendVariablesRead(names []string, node any) []string {
	items, isArray := node.([]any)
	if !isArray {
		return names
	}
	if len(items) == 2 && items[0] == "variable" {
		if name, isString := items[1].(string); isString {
			return append(names, name)
		}
	}
	for _, item := range items {
		names = appendVariablesRead(names, item)
	}
	return names
}

// repositoryJustfile is the repository's justfile, seen from this
// package's directory, where go test runs the tests.
var repositoryJustfile = filepath.Join("..", "..", "justfile")

// readJustfileDump returns the justfile as
// `just --dump --dump-format=json` describes it. The dump only parses the
// file: it runs no backtick and no recipe. The test skips when just is
// not on PATH.
func readJustfileDump(t *testing.T, justfile string) justDump {
	t.Helper()
	justPath, err := exec.LookPath("just")
	if err != nil {
		t.Skip("just is not on PATH; the justfile's recipes cannot be read")
	}
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
	return dump
}

// Every justfile recipe with a variadic parameter receives its
// arguments as positional arguments and never pastes the parameter into
// its command line, so each word a developer types reaches go test or
// rx as one word.
func TestJustfile_VariadicRecipesPassEachArgumentAsOneWord(t *testing.T) {
	dump := readJustfileDump(t, repositoryJustfile)

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

// Every {{…}} expression in a recipe's body is exactly one top-level
// variable assigned a string literal of the justfile. A recipe parameter
// reaches the shell as "$1" (with [positional-arguments]) and a value
// just computes, such as the version from `git describe`, as an exported
// variable read as "$NAME": either way the shell sees it as one word and
// never reads it as code.
func TestJustfile_RecipesPasteOnlyLiteralsIntoTheirCommandLines(t *testing.T) {
	violations, checked := literalPasteViolations(readJustfileDump(t, repositoryJustfile))

	for _, violation := range violations {
		t.Error(violation)
	}
	// The test recipes have pasted {{test_timeout}} from the start: none
	// found means the dump's format changed under the test.
	if checked == 0 {
		t.Fatal("the dump shows no {{…}} expression in any recipe; did the format of just --dump change?")
	}
}

// interpolations returns the {{…}} fragments of the recipe's body, in
// order, each as the dump gives it: a JSON array that holds the
// expression between {{ and }}. The text around them is left out.
func (r justRecipe) interpolations() []json.RawMessage {
	var found []json.RawMessage
	for _, line := range r.Body {
		for _, fragment := range line {
			var text string
			if json.Unmarshal(fragment, &text) == nil {
				continue
			}
			found = append(found, fragment)
		}
	}
	return found
}

// pastedVariable returns the name of the variable an interpolation
// fragment reads when its expression is exactly one variable,
// ["variable", name]. Any other expression gives false: a backtick
// (["evaluate", …]), a function call, a concatenation, a path join, a
// condition or a string literal, even one whose only variables are
// literals, and any shape a later just may dump.
func pastedVariable(fragment json.RawMessage) (string, bool) {
	var expressions []json.RawMessage
	if json.Unmarshal(fragment, &expressions) != nil || len(expressions) != 1 {
		return "", false
	}
	// A node with a nested array, such as ["call", "env_var",
	// ["variable", "x"]], does not decode into []string.
	var node []string
	if json.Unmarshal(expressions[0], &node) != nil || len(node) != 2 || node[0] != "variable" {
		return "", false
	}
	return node[1], true
}

// pasteViolation returns why the interpolation fragment in the recipe
// called name may not be pasted into a shell line, or "" when it reads
// exactly a top-level variable assigned a string literal.
func pasteViolation(name string, recipe justRecipe, assignments map[string]justAssignment, fragment json.RawMessage) string {
	variable, isVariable := pastedVariable(fragment)
	if !isVariable {
		return fmt.Sprintf("recipe %s pastes the expression %s into its command line; only a variable assigned a string literal may be pasted: export the value and read it as \"$NAME\"", name, fragment)
	}
	// A parameter hides the top-level variable of its name, so it is
	// looked up first.
	if recipe.hasParameter(variable) {
		return fmt.Sprintf("recipe %s pastes its parameter {{%s}} into its command line; give the recipe [positional-arguments] and read \"$1\"", name, variable)
	}
	if assignment, found := assignments[variable]; !found || !assignment.isLiteral() {
		return fmt.Sprintf("recipe %s pastes {{%s}}, a value just computes when it runs, into its command line; export it and read it as \"$NAME\"", name, variable)
	}
	return ""
}

// literalPasteViolations checks every {{…}} expression in the recipes of
// dump and returns one message for each that pastes anything but a
// top-level variable assigned a string literal, in the order of the
// recipes' names, with the number of expressions it checked.
func literalPasteViolations(dump justDump) (violations []string, checked int) {
	// Sorted names give the messages in the same order on every run.
	for _, name := range slices.Sorted(maps.Keys(dump.Recipes)) {
		recipe := dump.Recipes[name]
		for _, fragment := range recipe.interpolations() {
			checked++
			if violation := pasteViolation(name, recipe, dump.Assignments, fragment); violation != "" {
				violations = append(violations, violation)
			}
		}
	}
	return violations, checked
}

// appendToJustfile appends text to the justfile at path.
func appendToJustfile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := file.WriteString(text); err != nil {
		_ = file.Close() // the write error is the one to report
		t.Fatalf("append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// The guard refuses every {{…}} expression but one that reads exactly a
// variable assigned a string literal. A value just computes reaches the
// shell as code whatever else the expression holds: a backtick or a
// function needs no variable at all, and env_var of a literal variable
// reads the environment variable the literal names. Each case appends one
// recipe, with the variables it needs, to a copy of the repository
// justfile, which itself passes the guard.
func TestJustfilePasteGuard_RefusesEveryExpressionButALiteralVariable(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantRefused bool
	}{
		{"a backtick", "crafted:\n    echo '{{`git describe --tags`}}'\n", true},
		{"a function that reads no variable", "crafted:\n    echo '{{env_var_or_default(\"GITHUB_REF_NAME\", \"x\")}}'\n", true},
		{"a function of a literal variable", "crafted_name := \"GITHUB_REF_NAME\"\n\ncrafted:\n    echo '{{env_var(crafted_name)}}'\n", true},
		{"a literal variable joined to a backtick", "crafted_literal := \"v\"\n\ncrafted:\n    echo '{{crafted_literal + `git describe --tags`}}'\n", true},
		{"a condition on a literal variable", "crafted_literal := \"v\"\n\ncrafted:\n    echo '{{ if crafted_literal == \"v\" { `git describe --tags` } else { \"\" } }}'\n", true},
		{"a string literal", "crafted:\n    echo '{{\"text\"}}'\n", true},
		{"a variable just computes", "crafted_computed := `git describe --tags`\n\ncrafted:\n    echo '{{crafted_computed}}'\n", true},
		{"a recipe parameter", "crafted tag:\n    echo '{{tag}}'\n", true},
		{"a literal variable", "crafted_literal := \"v\"\n\ncrafted:\n    echo '{{crafted_literal}}'\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			justfile := copyJustfile(t, t.TempDir())
			appendToJustfile(t, justfile, "\n"+tc.text)

			violations, _ := literalPasteViolations(readJustfileDump(t, justfile))

			if !tc.wantRefused {
				if len(violations) != 0 {
					t.Errorf("the guard refused %q: %q", tc.text, violations)
				}
				return
			}
			if len(violations) != 1 || !strings.HasPrefix(violations[0], "recipe crafted ") {
				t.Errorf("the guard gave %q for %q; want one message about the recipe crafted", violations, tc.text)
			}
		})
	}
}
