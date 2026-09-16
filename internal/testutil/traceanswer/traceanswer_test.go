package traceanswer

import (
	"encoding/json"
	"strings"
	"testing"
)

// The text the answers below describe: line 2 starts at byte 7 and
// line 3 at byte 21.
const text = "LINE 1\nLINE 2 NEEDLE\nLINE 3\n"

// answer builds a decoded trace answer holding one match at byte 14
// (line 2) and one context line at byte 21 (line 3), with the numbers
// given. relative is the match's relative_line_number.
func answer(t *testing.T, matchLine, relative, contextLine int, extra string) map[string]any {
	t.Helper()
	contextRelative := contextLine
	if contextLine == -1 {
		contextRelative = 0
	}
	raw := `{
		"request_id": "r", "time": 0.5, "cli_command": "rx trace NEEDLE",
		"matches": [{"pattern": "p1", "file": "f1", "offset": 14,
			"relative_line_number": ` + itoa(relative) + `, "absolute_line_number": ` + itoa(matchLine) + `,
			"line_text": "LINE 2 NEEDLE` + extra + `", "submatches": []}],
		"context_lines": {"p1:f1:14": [{"relative_line_number": ` + itoa(contextRelative) + `,
			"absolute_line_number": ` + itoa(contextLine) + `, "line_text": "LINE 3", "absolute_offset": 21}]}
	}`
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return doc
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDifference(t *testing.T) {
	cases := []struct {
		name      string
		got, want func(t *testing.T) map[string]any
		wantDiff  string // a substring of the difference; "" when they agree
	}{
		{
			name:     "equal answers agree",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			wantDiff: "",
		},
		{
			name: "the fields that name the run are not compared",
			got: func(t *testing.T) map[string]any {
				doc := answer(t, 2, 2, 3, "")
				doc["request_id"], doc["time"], doc["cli_command"] = "other", 9.0, nil
				return doc
			},
			want:     func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			wantDiff: "",
		},
		{
			name:     "a -1 match number resolved to its line agrees",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, -1, 1, 3, "") },
			wantDiff: "",
		},
		{
			name:     "the resolved number may sit on either side",
			got:      func(t *testing.T) map[string]any { return answer(t, -1, 1, -1, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			wantDiff: "",
		},
		{
			name:     "a -1 match number resolved to the wrong line fails",
			got:      func(t *testing.T) map[string]any { return answer(t, 3, 3, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, -1, 1, 3, "") },
			wantDiff: "matches[0]: line 3 resolved at byte 14, which is on line 2",
		},
		{
			name:     "a -1 context number resolved to the wrong line fails",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 2, 2, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, 2, 2, -1, "") },
			wantDiff: "context_lines.p1:f1:14[0]: line 2 resolved at byte 21, which is on line 3",
		},
		{
			name:     "two different numbers fail even when one is right",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, 4, 4, 3, "") },
			wantDiff: "matches[0].absolute_line_number: got 2, want 4",
		},
		{
			name:     "a resolved number whose relative number differs fails",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 1, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, -1, 1, 3, "") },
			wantDiff: "matches[0].relative_line_number: the resolved side (got) has 1, want 2 like its absolute_line_number",
		},
		{
			name:     "two unresolved numbers keep their relative numbers equal",
			got:      func(t *testing.T) map[string]any { return answer(t, -1, 1, 3, "") },
			want:     func(t *testing.T) map[string]any { return answer(t, -1, 5, 3, "") },
			wantDiff: "matches[0].relative_line_number: got 1, want 5",
		},
		{
			name:     "any other field must be equal",
			got:      func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "!") },
			want:     func(t *testing.T) map[string]any { return answer(t, -1, 1, 3, "") },
			wantDiff: "matches[0].line_text",
		},
		{
			name: "a missing match fails",
			got: func(t *testing.T) map[string]any {
				doc := answer(t, 2, 2, 3, "")
				doc["matches"] = []any{}
				return doc
			},
			want:     func(t *testing.T) map[string]any { return answer(t, 2, 2, 3, "") },
			wantDiff: "matches: got 0 items, want 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := Difference(tc.got(t), tc.want(t), []byte(text))
			if tc.wantDiff == "" && diff != "" {
				t.Fatalf("the answers should agree, got difference %q", diff)
			}
			if tc.wantDiff != "" && !strings.Contains(diff, tc.wantDiff) {
				t.Fatalf("difference = %q, want it to contain %q", diff, tc.wantDiff)
			}
		})
	}
}

func TestUnnumberedLines(t *testing.T) {
	for _, tc := range []struct {
		name               string
		matchLine, context int
		want               int
	}{
		{"every line numbered", 2, 3, 0},
		{"the match unnumbered", -1, 3, 1},
		{"match and context unnumbered", -1, -1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnnumberedLines(answer(t, tc.matchLine, 1, tc.context, "")); got != tc.want {
				t.Errorf("UnnumberedLines = %d, want %d", got, tc.want)
			}
		})
	}
}

// A struct answer is compared as the JSON document it encodes to.
func TestDifferenceAcceptsAnyValueThatEncodesToAnAnswer(t *testing.T) {
	type match struct {
		Offset             int64 `json:"offset"`
		AbsoluteLineNumber int   `json:"absolute_line_number"`
	}
	type doc struct {
		Matches []match `json:"matches"`
	}
	got := doc{Matches: []match{{Offset: 14, AbsoluteLineNumber: 2}}}
	want := answer(t, 2, 2, 3, "")
	if diff := Difference(got, want, []byte(text)); !strings.Contains(diff, "context_lines") {
		t.Errorf("difference = %q, want one naming the missing context_lines", diff)
	}
}
