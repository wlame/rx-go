package clicommand

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --offsets and --lines accept the same two spellings in both backends:
// a comma-separated list in one flag, and the flag repeated. rx-go took
// only the first and rx-python only the second, so a script that named
// several positions worked against exactly one of them, and the failure
// was either a parse error or — worse here — a silently dropped value.

func multiValueFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\ndelta\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func decodeSamples(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return decoded
}

func TestSamples_RepeatedAndCommaSeparatedAgree(t *testing.T) {
	path := multiValueFixture(t)

	cases := []struct {
		name   string
		params samplesParams
	}{
		{"offsets comma", samplesParams{path: path, offsets: []string{"0,6"}, ctxLines: 0, jsonOutput: true}},
		{"offsets repeated", samplesParams{path: path, offsets: []string{"0", "6"}, ctxLines: 0, jsonOutput: true}},
		{"lines comma", samplesParams{path: path, lines: []string{"1,2"}, ctxLines: 0, jsonOutput: true}},
		{"lines repeated", samplesParams{path: path, lines: []string{"1", "2"}, ctxLines: 0, jsonOutput: true}},
	}

	results := map[string]map[string]any{}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := runSamples(&buf, tc.params); err != nil {
			t.Fatalf("%s: runSamples: %v", tc.name, err)
		}
		results[tc.name] = decodeSamples(t, buf.Bytes())
	}

	offsetKeys := results["offsets comma"]["offsets"].(map[string]any)
	if len(offsetKeys) != 2 {
		t.Errorf("offsets comma: got %d entries, want 2: %v", len(offsetKeys), offsetKeys)
	}
	if !jsonEqual(results["offsets comma"], results["offsets repeated"]) {
		t.Errorf("comma and repeated --offsets disagree:\n %v\n %v",
			results["offsets comma"], results["offsets repeated"])
	}
	if !jsonEqual(results["lines comma"], results["lines repeated"]) {
		t.Errorf("comma and repeated --lines disagree:\n %v\n %v",
			results["lines comma"], results["lines repeated"])
	}
}

// A repeated flag adds to the list rather than replacing it, which is
// the bug this covers: --offsets=0 --offsets=6 used to answer only 6.
func TestSamples_RepeatedFlagDoesNotReplaceEarlierValues(t *testing.T) {
	path := multiValueFixture(t)

	var buf bytes.Buffer
	err := runSamples(&buf, samplesParams{
		path:       path,
		offsets:    []string{"0", "6", "11"},
		jsonOutput: true,
	})
	if err != nil {
		t.Fatalf("runSamples: %v", err)
	}

	offsets := decodeSamples(t, buf.Bytes())["offsets"].(map[string]any)
	for _, key := range []string{"0", "6", "11"} {
		if _, ok := offsets[key]; !ok {
			t.Errorf("offset %s is missing: %v", key, offsets)
		}
	}
}

func jsonEqual(a, b map[string]any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(left, right)
}
