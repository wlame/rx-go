package webapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// sandboxedFiles writes each named file into one fresh search root and
// returns their paths, keyed by name.
func sandboxedFiles(t *testing.T, contents map[string]string) map[string]string {
	t.Helper()
	root := t.TempDir()
	written := make(map[string]string, len(contents))
	for name, content := range contents {
		file := filepath.Join(root, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		written[name] = file
	}
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)
	return written
}

// describedMatches renders each match as "pattern text|file name|line",
// resolving the response's pattern and file IDs, so a case can list the
// matches it expects without knowing the IDs.
func describedMatches(body rxtypes.TraceResponse) []string {
	described := make([]string, 0, len(body.Matches))
	for _, m := range body.Matches {
		file := filepath.Base(body.Files[m.File])
		described = append(described, fmt.Sprintf("%s|%s|%d", body.Patterns[m.Pattern], file, m.AbsoluteLineNumber))
	}
	slices.Sort(described)
	return described
}

// patternTexts lists the patterns a response searched for, sorted.
func patternTexts(body rxtypes.TraceResponse) []string {
	texts := make([]string, 0, len(body.Patterns))
	for _, text := range body.Patterns {
		texts = append(texts, text)
	}
	slices.Sort(texts)
	return texts
}

// An array query parameter takes one value per repetition, and a comma
// inside a value is part of that value: a counted repetition stays one
// pattern and a file name with a comma stays one path.
func TestTrace_ArrayParametersTakeRepeatedValuesAndKeepCommas(t *testing.T) {
	files := sandboxedFiles(t, map[string]string{
		"one.log":        "a\naaa\nb only\n",
		"two.log":        "zzz\nbbb here\naaaa\n",
		"with,comma.log": "nothing\naaa in the comma file\n",
	})
	ts := newServerWithRipgrep(t)

	cases := []struct {
		name         string
		paths        []string
		patterns     []string
		wantPatterns []string
		wantMatches  []string
	}{
		{
			name:         "counted repetition is one pattern",
			paths:        []string{"one.log"},
			patterns:     []string{"a{2,5}"},
			wantPatterns: []string{"a{2,5}"},
			wantMatches:  []string{"a{2,5}|one.log|2"},
		},
		{
			name:         "repeated regexp searches every pattern",
			paths:        []string{"two.log"},
			patterns:     []string{"aaa", "bbb"},
			wantPatterns: []string{"aaa", "bbb"},
			wantMatches:  []string{"aaa|two.log|3", "bbb|two.log|2"},
		},
		{
			name:         "repeated path searches every file",
			paths:        []string{"one.log", "two.log"},
			patterns:     []string{"aaa"},
			wantPatterns: []string{"aaa"},
			wantMatches:  []string{"aaa|one.log|2", "aaa|two.log|3"},
		},
		{
			name:         "path with a comma is one path",
			paths:        []string{"with,comma.log"},
			patterns:     []string{"aaa"},
			wantPatterns: []string{"aaa"},
			wantMatches:  []string{"aaa|with,comma.log|2"},
		},
		{
			name:         "two paths and two patterns, as the viewer sends them",
			paths:        []string{"one.log", "two.log"},
			patterns:     []string{"aaa", "bbb"},
			wantPatterns: []string{"aaa", "bbb"},
			wantMatches:  []string{"aaa|one.log|2", "aaa|two.log|3", "bbb|two.log|2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantPaths := make([]string, 0, len(tc.paths))
			for _, name := range tc.paths {
				wantPaths = append(wantPaths, files[name])
			}
			query := url.Values{"path": wantPaths, "regexp": tc.patterns}

			body := getTrace(t, ts.URL, query)

			if !slices.Equal(body.Path, wantPaths) {
				t.Errorf("path %q, want %q", body.Path, wantPaths)
			}
			if got := patternTexts(body); !slices.Equal(got, tc.wantPatterns) {
				t.Errorf("patterns %q, want %q", got, tc.wantPatterns)
			}
			if got := describedMatches(body); !slices.Equal(got, tc.wantMatches) {
				t.Errorf("matches %q, want %q", got, tc.wantMatches)
			}
		})
	}
}

// Every array query parameter in the published contract is declared
// exploded (form style, one value per repetition), so a client generator
// sends ?p=a&p=b and the server reads it that way. An array parameter
// added later without the explode option fails here.
func TestOpenAPI_EveryArrayQueryParameterIsExploded(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name    string `json:"name"`
				In      string `json:"in"`
				Explode *bool  `json:"explode"`
				Style   string `json:"style"`
				Schema  struct {
					Type any `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	arrayParams := 0
	for path, operations := range doc.Paths {
		for method, op := range operations {
			for _, p := range op.Parameters {
				if p.In != "query" || !schemaTypeIncludes(p.Schema.Type, "array") {
					continue
				}
				arrayParams++
				if p.Explode == nil || !*p.Explode {
					t.Errorf("%s %s: array query parameter %q is not exploded", method, path, p.Name)
				}
				if p.Style != "" && p.Style != "form" {
					t.Errorf("%s %s: array query parameter %q has style %q, want form", method, path, p.Name, p.Style)
				}
			}
		}
	}
	if arrayParams == 0 {
		t.Fatal("the golden spec has no array query parameter; the walk read nothing")
	}
}

// schemaTypeIncludes reports whether a JSON Schema "type" — a single name
// or, in OpenAPI 3.1, a list such as ["array", "null"] — names want.
func schemaTypeIncludes(schemaType any, want string) bool {
	switch typed := schemaType.(type) {
	case string:
		return typed == want
	case []any:
		return slices.Contains(typed, any(want))
	}
	return false
}
