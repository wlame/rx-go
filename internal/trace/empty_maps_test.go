package trace

import (
	"context"
	"encoding/json"
	"testing"
)

// TestEngine_EmptyMapsMarshalAsObjects pins the two map fields of a trace
// answer to `{}` when they have nothing in them. The published schema
// declares both as non-null objects, so a client generated from it
// rejects a `null`: a search with no match (and no context requested)
// has no context lines, and a request whose paths hold no file to scan
// has no chunks either.
func TestEngine_EmptyMapsMarshalAsObjects(t *testing.T) {
	requireRipgrep(t)
	t.Setenv("RX_CACHE_DIR", t.TempDir())

	cases := []struct {
		name             string
		paths            []string
		wantFileChunks   string
		wantContextLines string
	}{
		{"no match in a file", []string{mustWriteFile(t, []byte("alpha\nbeta\n"))}, `{"f1":1}`, `{}`},
		{"nothing to scan", []string{t.TempDir()}, `{}`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := New().RunWithOptions(context.Background(), tc.paths,
				[]string{"zzqqxx"}, Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			body, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := string(fields["file_chunks"]); got != tc.wantFileChunks {
				t.Errorf("file_chunks = %s, want %s", got, tc.wantFileChunks)
			}
			if got := string(fields["context_lines"]); got != tc.wantContextLines {
				t.Errorf("context_lines = %s, want %s", got, tc.wantContextLines)
			}
		})
	}
}
