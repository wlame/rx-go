package webapi

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
)

// traceFindingsMembers are the members a trace with signatures adds to
// the trace answer.
var traceFindingsMembers = []string{"signatures", "findings", "finding_summary"}

// goldenSchemas reads the components of the golden OpenAPI document.
func goldenSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Components.Schemas
}

// Without its three optional members the trace answer is the answer of
// contract 1.7, schema for schema (the trace answer and every schema
// it refers to, compared parsed): a client of 1.7 reads a trace of 1.8
// unchanged, and none of the three is required.
func TestOpenAPI_TraceResponseWithoutSignaturesIsTheContract17Answer(t *testing.T) {
	raw, err := os.ReadFile("testdata/trace-response-schemas-1.7.json")
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]map[string]any
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	now := goldenSchemas(t)
	trace := maps.Clone(now["TraceResponse"])
	props := maps.Clone(trace["properties"].(map[string]any))
	for _, member := range traceFindingsMembers {
		if _, ok := props[member]; !ok {
			t.Errorf("TraceResponse lacks %s", member)
		}
		delete(props, member)
		if slices.Contains(trace["required"].([]any), any(member)) {
			t.Errorf("TraceResponse requires %s", member)
		}
	}
	trace["properties"] = props
	now["TraceResponse"] = trace
	for name, want := range old {
		if !reflect.DeepEqual(now[name], want) {
			got, _ := json.MarshalIndent(now[name], "", "  ")
			t.Errorf("%s differs from contract 1.7:\n%s", name, got)
		}
	}
}

// The members a trace with signatures adds have the shapes the
// contract gives them: findings an array (never null) of Finding,
// signatures and finding_summary objects of SignatureRef and
// FileFindingSummary; a finding's time_ms may be null, its optional
// members are not required.
func TestOpenAPI_TraceFindingsMembers(t *testing.T) {
	schemas := goldenSchemas(t)
	props := schemas["TraceResponse"]["properties"].(map[string]any)
	findings := props["findings"].(map[string]any)
	if findings["type"] != "array" || findings["items"].(map[string]any)["$ref"] != "#/components/schemas/Finding" {
		t.Errorf("findings %v", findings)
	}
	for member, ref := range map[string]string{"signatures": "SignatureRef", "finding_summary": "FileFindingSummary"} {
		got := props[member].(map[string]any)["additionalProperties"].(map[string]any)["$ref"]
		if got != "#/components/schemas/"+ref {
			t.Errorf("%s values %v, want %s", member, got, ref)
		}
	}
	finding := schemas["Finding"]
	required := finding["required"].([]any)
	for _, want := range []string{"signature", "start_line", "end_line", "start_offset", "end_offset", "time_ms", "excerpt"} {
		if !slices.Contains(required, any(want)) {
			t.Errorf("Finding does not require %s", want)
		}
	}
	for _, optional := range []string{"file", "value_start", "value_end", "masked", "lines", "before", "after", "lines_truncated"} {
		if slices.Contains(required, any(optional)) {
			t.Errorf("Finding requires the optional %s", optional)
		}
	}
	timeMS := finding["properties"].(map[string]any)["time_ms"].(map[string]any)
	if !reflect.DeepEqual(timeMS["type"], []any{"integer", "null"}) {
		t.Errorf("time_ms type %v, want integer or null", timeMS["type"])
	}
}
