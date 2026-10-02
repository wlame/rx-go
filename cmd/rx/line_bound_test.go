package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A line longer than RX_MAX_LINE_TEXT_BYTES is answered alike by
// GET /v1/trace and by the `rx trace --json` command the server renders
// for it: the same cut text and submatches, the same flags, the same
// offsets and numbers, in the match and in the windows around it.
func TestTrace_HTTPAndTheCLIBoundALongLineAlike(t *testing.T) {
	t.Setenv("RX_MAX_LINE_TEXT_BYTES", "4096")
	t.Setenv("RX_MAX_SUBMATCHES_PER_LINE", "100")
	f := startRouterFixture(t)
	var text strings.Builder
	text.WriteString("LINE 1 " + strings.Repeat("x", 200_000) + "\n")
	for i := 2; i <= 50; i++ {
		text.WriteString("LINE " + strconv.Itoa(i) + " x short\n")
	}
	path := filepath.Join(f.root, "long.log")
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	answer := f.get(t, "/v1/trace", url.Values{"path": {path}, "regexp": {"x"}, "context": {"1"}})

	matches, _ := answer["matches"].([]any)
	if len(matches) != 50 {
		t.Fatalf("%d matches, want 50", len(matches))
	}
	first, _ := matches[0].(map[string]any)
	lineText, _ := first["line_text"].(string)
	submatches, _ := first["submatches"].([]any)
	if len(lineText) != 4096 || first["line_text_truncated"] != true || len(submatches) != 100 || first["submatches_truncated"] != true {
		t.Fatalf("first match: %d bytes of text (truncated %v), %d submatches (truncated %v); want 4096, true, 100, true",
			len(lineText), first["line_text_truncated"], len(submatches), first["submatches_truncated"])
	}
	rendered, _ := answer["cli_command"].(string)
	requireSameAnswer(t, answer, f.runRenderedJSON(t, requireRunnableCommand(t, rendered)))
}
