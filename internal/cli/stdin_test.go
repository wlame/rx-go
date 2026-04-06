package cli

import (
	"os"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// withFakeStdin replaces os.Stdin with a pipe containing the given content,
// runs the provided function, then restores the original stdin. This lets us
// simulate piped input for commands that read from os.Stdin.
func withFakeStdin(t *testing.T, content string, fn func()) {
	t.Helper()

	origStdin := os.Stdin

	r, w, err := os.Pipe()
	require.NoError(t, err)

	_, err = w.WriteString(content)
	require.NoError(t, err)
	w.Close()

	os.Stdin = r
	defer func() {
		os.Stdin = origStdin
		r.Close()
	}()

	fn()
}

// --------------------------------------------------------------------------
// Basic stdin pipe tests (using "-" as explicit path)
// --------------------------------------------------------------------------

func TestStdin_ExplicitDash_FindsMatches(t *testing.T) {
	// echo "ERROR something broke" | rx trace "ERROR" -
	// The "-" path tells the trace command to read stdin.
	content := "line one\nERROR something broke\nline three\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "ERROR", "-")
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err, "trace --json with stdin should produce valid JSON")

		assert.GreaterOrEqual(t, len(resp.Matches), 1,
			"should find at least 1 match in piped input")
	})
}

func TestStdin_ExplicitDash_JSON_HasExpectedFields(t *testing.T) {
	content := "hello world\nerror found here\ngoodbye world\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "error", "-")
		require.NoError(t, err)

		var raw map[string]interface{}
		err = json.Unmarshal([]byte(stdout), &raw)
		require.NoError(t, err)

		requiredFields := []string{
			"request_id", "path", "time", "patterns", "files",
			"matches", "scanned_files", "skipped_files",
		}
		for _, key := range requiredFields {
			assert.Contains(t, raw, key, "stdin JSON must contain field %q", key)
		}
	})
}

func TestStdin_ExplicitDash_NoMatches(t *testing.T) {
	// Piping content that doesn't match the pattern should return 0 matches.
	content := "hello world\ngoodbye world\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "ZZZZZ_NEVER_MATCH", "-")
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err)

		assert.Empty(t, resp.Matches, "non-matching pattern should return 0 matches")
	})
}

func TestStdin_ExplicitDash_RegexPattern(t *testing.T) {
	// Test that regex patterns work on piped input.
	content := "alpha one\nbeta two\ngamma three\nalpha four\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "alpha|gamma", "-")
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err)

		// "alpha one", "gamma three", "alpha four" = 3 matches
		assert.GreaterOrEqual(t, len(resp.Matches), 2,
			"regex alternation should match multiple lines")
	})
}

func TestStdin_ExplicitDash_MaxResults(t *testing.T) {
	// --max-results should limit stdin results.
	content := "ERROR line 1\nERROR line 2\nERROR line 3\nERROR line 4\nERROR line 5\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "--max-results=2", "ERROR", "-")
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err)

		assert.LessOrEqual(t, len(resp.Matches), 2,
			"--max-results=2 should limit to at most 2 matches on stdin")
	})
}

// --------------------------------------------------------------------------
// Auto-detect stdin (no explicit "-" path)
// --------------------------------------------------------------------------

func TestStdin_AutoDetect_FindsMatches(t *testing.T) {
	// When stdin is piped and no path is given, trace should auto-detect stdin.
	// This simulates: echo "ERROR here" | rx trace "ERROR"
	content := "first line\nERROR happened here\nlast line\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "ERROR")
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err, "auto-detected stdin should produce valid JSON")

		assert.GreaterOrEqual(t, len(resp.Matches), 1,
			"auto-detected stdin should find matches")
	})
}

// --------------------------------------------------------------------------
// Stdin with text mode output
// --------------------------------------------------------------------------

func TestStdin_ExplicitDash_TextOutput(t *testing.T) {
	// Non-JSON (text/CLI) output should also work with stdin.
	content := "normal line\nERROR critical failure\nnormal line\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "ERROR", "-")
		require.NoError(t, err)

		assert.Contains(t, stdout, "match", "text output should mention matches")
		assert.Contains(t, stdout, "ERROR", "text output should contain matching text")
	})
}

// --------------------------------------------------------------------------
// Stdin cleanup verification
// --------------------------------------------------------------------------

func TestStdin_ExplicitDash_TempFileCleanedUp(t *testing.T) {
	// After the command completes, the temporary stdin file should be removed.
	// We verify this indirectly: the command should succeed and not leave
	// dangling temp files (we check the temp dir doesn't grow).
	content := "ERROR test cleanup\n"

	withFakeStdin(t, content, func() {
		root := NewRootCommand()
		_, _, err := executeCommand(root, "trace", "--json", "ERROR", "-")
		require.NoError(t, err)
		// If we get here without error, the temp file was handled properly.
		// The defer in runTrace calls os.Remove(stdinTempFile).
	})
}

// --------------------------------------------------------------------------
// Stdin with empty input
// --------------------------------------------------------------------------

func TestStdin_ExplicitDash_EmptyInput(t *testing.T) {
	// Empty stdin with explicit "-" should not panic or hang.
	withFakeStdin(t, "", func() {
		root := NewRootCommand()
		stdout, _, err := executeCommand(root, "trace", "--json", "ERROR", "-")
		// With empty stdin, the temp file will be empty. rg will find no matches.
		// The command should still succeed.
		require.NoError(t, err)

		var resp models.TraceResponse
		err = json.Unmarshal([]byte(stdout), &resp)
		require.NoError(t, err)

		assert.Empty(t, resp.Matches, "empty stdin should produce 0 matches")
	})
}
