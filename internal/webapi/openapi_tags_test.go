package webapi

import (
	"encoding/json"
	"testing"
)

// Every tag an operation carries is declared in the document's tags
// list with a description, so a renderer never groups operations under
// an undescribed heading.
func TestOpenAPI_EveryOperationTagIsDeclared(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := get(t, ts.URL+"/openapi.json", "")
	var doc struct {
		Tags  []struct{ Name, Description string } `json:"tags"`
		Paths map[string]map[string]struct {
			Tags []string `json:"tags"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	declared := map[string]bool{}
	for _, tag := range doc.Tags {
		if tag.Description == "" {
			t.Errorf("tag %q has no description", tag.Name)
		}
		declared[tag.Name] = true
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			for _, tag := range op.Tags {
				if !declared[tag] {
					t.Errorf("%s %s carries the undeclared tag %q", method, path, tag)
				}
			}
		}
	}
	if !declared["Signatures"] {
		t.Error("the Signatures tag is not declared")
	}
}
