// Package traceanswer compares two trace answers under the rule that
// binds an accelerator (a line index, the trace cache) and --no-index:
// every field of the two answers is equal, except that a line number
// which is -1 in one answer may be the true line number in the other.
//
// -1 means "not computed": a scan cut short by a max_results cap leaves
// the lines it could not number at -1, and an accelerator, or the line
// count --no-index makes, may fill them in. A number that is filled in
// must be the line that holds the match's byte offset in the file's
// text, and the relative_line_number beside it carries the same value.
// The -1 side's relative_line_number is chunk-relative and is not
// compared. Two numbers that are both known must be equal.
//
// Answers are compared as parsed JSON documents, never as bytes: object
// key order is not part of the contract. The fields that name the run
// (request_id, time, cli_command) are left out.
//
// This is test-only infrastructure. It lives under internal/testutil so
// the trace engine's tests and the CLI's binary tests share one
// statement of the rule, and it must not be imported from non-test
// code.
package traceanswer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
)

// unknownLine is the line number of a line a capped scan left
// unnumbered.
const unknownLine = -1

// runFields are the top-level fields that differ between two runs of
// one request; they are never compared.
var runFields = []string{"request_id", "time", "cli_command"}

// offsetFields are the fields that hold a numbered object's byte offset:
// "offset" on a match and "absolute_offset" on a context line.
var offsetFields = []string{"offset", "absolute_offset"}

// Difference names the first place where got and want disagree under
// the rule in the package comment, or returns "" when they agree.
//
// got and want are trace answers in any form that encodes to the trace
// JSON document: a *rxtypes.TraceResponse, a decoded map[string]any, or
// a struct holding a subset of the fields. text is the file's text, the
// decompressed stream for a compressed file; a line number that one side
// fills in is checked against it. With text nil, any -1 that the other
// side fills in is a difference.
func Difference(got, want any, text []byte) string {
	g, err := document(got)
	if err != nil {
		return fmt.Sprintf("got: %v", err)
	}
	w, err := document(want)
	if err != nil {
		return fmt.Sprintf("want: %v", err)
	}
	return difference("", g, w, text)
}

// UnnumberedLines counts the matches and context lines of answer whose
// absolute_line_number is -1. A completed scan answers with none.
func UnnumberedLines(answer any) int {
	doc, err := document(answer)
	if err != nil {
		return 0
	}
	return countUnnumbered(doc)
}

// RequireAgree fails t when got and want disagree under the rule in the
// package comment. name says which pair of answers is compared, such as
// "cache hit" or "--no-index".
func RequireAgree(t testing.TB, name string, got, want any, text []byte) {
	t.Helper()
	if diff := Difference(got, want, text); diff != "" {
		t.Fatalf("%s: the answers differ at %s", name, diff)
	}
}

// RequireSame fails t unless got and want are equal in every field. It
// is the rule for two answers of a scan that ran to the end: both
// number every line, so the -1 exception cannot apply, and a -1 in
// either answer fails on its own.
func RequireSame(t testing.TB, name string, got, want any) {
	t.Helper()
	for side, answer := range map[string]any{"got": got, "want": want} {
		if n := UnnumberedLines(answer); n > 0 {
			t.Fatalf("%s: %s leaves %d line number(s) at -1; a completed scan numbers every line", name, side, n)
		}
	}
	RequireAgree(t, name, got, want, nil)
}

// document turns an answer into its parsed JSON document, without the
// fields that name the run.
func document(answer any) (any, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encode the answer: %w", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode the answer: %w", err)
	}
	if top, ok := doc.(map[string]any); ok {
		for _, field := range runFields {
			delete(top, field)
		}
	}
	return doc, nil
}

// difference walks got and want together and names the first place
// they disagree. path is where the walk is, as "matches[3].line_text".
func difference(path string, got, want any, text []byte) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: got %.200v, want an object", where(path), got)
		}
		if _, numbered := w["absolute_line_number"]; numbered {
			return numberedDifference(path, g, w, text)
		}
		return objectDifference(path, g, w, text, nil)
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: got %.200v, want a list", where(path), got)
		}
		for i := range min(len(g), len(w)) {
			if diff := difference(fmt.Sprintf("%s[%d]", path, i), g[i], w[i], text); diff != "" {
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

// objectDifference compares two objects key by key, in sorted key
// order, leaving out the keys in skip. Both objects must have the same
// keys, skipped ones included.
func objectDifference(path string, got, want map[string]any, text []byte, skip []string) string {
	gotKeys, wantKeys := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))
	if !slices.Equal(gotKeys, wantKeys) {
		return fmt.Sprintf("%s: got keys %v, want keys %v", where(path), gotKeys, wantKeys)
	}
	for _, key := range wantKeys {
		if slices.Contains(skip, key) {
			continue
		}
		if diff := difference(join(path, key), got[key], want[key], text); diff != "" {
			return diff
		}
	}
	return ""
}

// numberedDifference compares two matches, or two context lines: the
// objects that carry a line number. Equal numbers leave every field to
// compare. Otherwise one side must be -1 and the other the line that
// holds the object's byte offset in text, with its relative number set
// to the same line; the -1 side's relative number is chunk-relative and
// is not compared.
func numberedDifference(path string, got, want map[string]any, text []byte) string {
	gotLine, gotOK := number(got["absolute_line_number"])
	wantLine, wantOK := number(want["absolute_line_number"])
	if !gotOK || !wantOK || gotLine == wantLine {
		return objectDifference(path, got, want, text, nil)
	}
	if gotLine != unknownLine && wantLine != unknownLine {
		return fmt.Sprintf("%s.absolute_line_number: got %d, want %d", path, gotLine, wantLine)
	}
	resolvedSide, resolved, line := "got", got, gotLine
	if gotLine == unknownLine {
		resolvedSide, resolved, line = "want", want, wantLine
	}
	offset, ok := byteOffset(resolved)
	if !ok {
		return fmt.Sprintf("%s: line %d resolved, but the object has no byte offset", path, line)
	}
	if diff := resolvedLineDifference(path, line, offset, text); diff != "" {
		return diff
	}
	if relative, _ := number(resolved["relative_line_number"]); relative != line {
		return fmt.Sprintf("%s.relative_line_number: the resolved side (%s) has %v, want %d like its absolute_line_number",
			path, resolvedSide, resolved["relative_line_number"], line)
	}
	return objectDifference(path, got, want, text, []string{"absolute_line_number", "relative_line_number"})
}

// resolvedLineDifference checks a line number that one answer filled in
// where the other left -1: it must be the line holding offset in text.
func resolvedLineDifference(path string, line int, offset int64, text []byte) string {
	if text == nil {
		return fmt.Sprintf("%s: line %d resolved at byte %d where the other answer has -1, and no text was given to check it",
			path, line, offset)
	}
	if offset < 0 || offset >= int64(len(text)) {
		return fmt.Sprintf("%s: line %d resolved at byte %d, past the end of the %d-byte text", path, line, offset, len(text))
	}
	if actual := bytes.Count(text[:offset], []byte{'\n'}) + 1; actual != line {
		return fmt.Sprintf("%s: line %d resolved at byte %d, which is on line %d", path, line, offset, actual)
	}
	return ""
}

// countUnnumbered counts the objects under doc whose
// absolute_line_number is -1.
func countUnnumbered(doc any) int {
	count := 0
	switch v := doc.(type) {
	case map[string]any:
		if line, ok := number(v["absolute_line_number"]); ok && line == unknownLine {
			count++
		}
		for _, child := range v {
			count += countUnnumbered(child)
		}
	case []any:
		for _, child := range v {
			count += countUnnumbered(child)
		}
	}
	return count
}

// byteOffset is the byte offset a match or a context line carries.
func byteOffset(object map[string]any) (int64, bool) {
	for _, field := range offsetFields {
		if n, ok := number(object[field]); ok {
			return int64(n), true
		}
	}
	return 0, false
}

// number reads a JSON number, which encoding/json decodes as float64.
func number(value any) (int, bool) {
	f, ok := value.(float64)
	return int(f), ok
}

// join appends an object key to a path.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// where names a path in a message; the empty path is the whole answer.
func where(path string) string {
	if path == "" {
		return "the answer"
	}
	return path
}
