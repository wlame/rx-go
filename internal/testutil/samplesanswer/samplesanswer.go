// Package samplesanswer compares samples answers under the rule that
// binds a line index: an index is an accelerator, so an answer reached
// with one is equal, field by field, to the answer reached without it.
//
// Unlike a trace answer, a samples answer has no "not computed" line
// number: a -1 there means the position is not in the file, which an
// index cannot change. So the rule is plain equality, with one field
// left out: cli_command, which describes how an answer was asked for
// rather than the answer.
//
// Answers are compared as parsed JSON documents, never as bytes: object
// key order is not part of the contract.
//
// ColdAndIndexed is the check every samples scenario goes through: it
// asks once with the cache empty, builds and stores the file's line
// index, asks again and requires the two answers to agree. It takes the
// request as a function rather than importing internal/samples, so the
// samples package's own tests can use it without an import cycle.
//
// This is test-only infrastructure. It lives under internal/testutil so
// the samples, CLI and HTTP tests share one statement of the rule, and
// it must not be imported from non-test code.
package samplesanswer

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/index"
)

// productionFields are the top-level fields that describe how an answer
// was asked for, not the answer: they are never compared.
var productionFields = []string{"cli_command"}

// Difference names the first place where got and want disagree, as
// "samples.12[3]: got …, want …", or returns "" when they are equal in
// every field but productionFields.
//
// got and want are samples answers in any form that encodes to the
// samples JSON document: a *rxtypes.SamplesResponse, a decoded
// map[string]any, or the raw bytes of a --json output (json.RawMessage).
func Difference(got, want any) string {
	g, err := document(got)
	if err != nil {
		return fmt.Sprintf("got: %v", err)
	}
	w, err := document(want)
	if err != nil {
		return fmt.Sprintf("want: %v", err)
	}
	return difference("", g, w)
}

// RequireAgree fails t when got and want disagree (Difference). name
// says which pair of answers is compared, such as "gzip copy".
func RequireAgree(t testing.TB, name string, got, want any) {
	t.Helper()
	if diff := Difference(got, want); diff != "" {
		t.Fatalf("%s: the answers differ at %s", name, diff)
	}
}

// ColdAndIndexed asks for a samples answer twice and requires the two
// to agree: first with no line index stored for path, then after
// building and storing one with checkpoints every stepBytes bytes (0
// takes the builder's default step). It returns the first answer.
//
// answer performs the request however the caller's surface does it (the
// resolver, the CLI, the HTTP router) and returns the answer; it must
// read the stored index the way that surface does, so the second call
// is the indexed one. The test fails when an index is already stored
// before the first call, since that answer would not be cold, and when
// the index could not be built or stored.
func ColdAndIndexed(t testing.TB, path string, stepBytes int64, answer func(t testing.TB) any) any {
	t.Helper()
	if stored, _ := index.LoadForSource(path); stored != nil {
		t.Fatalf("an index is already stored for %s, so the first answer would not be cold", path)
	}
	cold := answer(t)

	idx, err := index.Build(path, index.BuildOptions{StepBytes: stepBytes})
	if err != nil {
		t.Fatalf("build the index of %s: %v", path, err)
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("store the index of %s: %v", path, err)
	}
	if stored, err := index.LoadForSource(path); stored == nil {
		t.Fatalf("the index of %s was stored but does not load: %v", path, err)
	}

	indexed := answer(t)
	RequireAgree(t, "cold and indexed", indexed, cold)
	return cold
}

// document turns an answer into its parsed JSON document, without the
// production fields.
func document(answer any) (any, error) {
	raw, ok := answer.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(answer); err != nil {
			return nil, fmt.Errorf("encode the answer: %w", err)
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode the answer: %w", err)
	}
	if top, ok := doc.(map[string]any); ok {
		for _, field := range productionFields {
			delete(top, field)
		}
	}
	return doc, nil
}

// difference walks got and want together and names the first place
// they disagree. path is where the walk is, as "samples.12[3]".
func difference(path string, got, want any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: got %.200v, want an object", where(path), got)
		}
		gotKeys, wantKeys := slices.Sorted(maps.Keys(g)), slices.Sorted(maps.Keys(w))
		if !slices.Equal(gotKeys, wantKeys) {
			return fmt.Sprintf("%s: got keys %v, want keys %v", where(path), gotKeys, wantKeys)
		}
		for _, key := range wantKeys {
			if diff := difference(join(path, key), g[key], w[key]); diff != "" {
				return diff
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: got %.200v, want a list", where(path), got)
		}
		for i := range min(len(g), len(w)) {
			if diff := difference(fmt.Sprintf("%s[%d]", path, i), g[i], w[i]); diff != "" {
				return diff
			}
		}
		if len(g) != len(w) {
			return fmt.Sprintf("%s: got %d items, want %d", where(path), len(g), len(w))
		}
	default:
		if !reflect.DeepEqual(got, want) {
			return fmt.Sprintf("%s: got %.200v, want %.200v", where(path), got, want)
		}
	}
	return ""
}

// join appends an object key to a path.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// where names the top of the document for an empty path.
func where(path string) string {
	if path == "" {
		return "the answer"
	}
	return path
}
