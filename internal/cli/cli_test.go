package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// executeCommand sets args on a root command, captures stdout+stderr, and executes.
// Returns stdout content, stderr content, and any error.
func executeCommand(root *cobra.Command, args ...string) (stdout string, stderr string, err error) {
	var stdoutBuf, stderrBuf bytes.Buffer
	root.SetOut(&stdoutBuf)
	root.SetErr(&stderrBuf)
	root.SetArgs(args)

	err = root.Execute()
	return stdoutBuf.String(), stderrBuf.String(), err
}

// createTempFile creates a temp file with the given content and returns its path.
// The caller is responsible for cleanup via t.Cleanup or defer os.Remove.
func createTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)
	return path
}

// --------------------------------------------------------------------------
// Root / default routing tests
// --------------------------------------------------------------------------

func TestShouldRouteToTrace_BarePattern(t *testing.T) {
	// `rx "error" file` — first arg is not a known subcommand and not a flag.
	assert.True(t, shouldRouteToTrace([]string{"error", "file.log"}))
}

func TestShouldRouteToTrace_FlagBeforePattern(t *testing.T) {
	// `rx --json "error" file` — first arg is a flag, but a non-subcommand arg follows.
	assert.True(t, shouldRouteToTrace([]string{"--json", "error", "file.log"}))
}

func TestShouldRouteToTrace_ExplicitTrace(t *testing.T) {
	// `rx trace "error" file` — first arg is a known subcommand.
	assert.False(t, shouldRouteToTrace([]string{"trace", "error", "file.log"}))
}

func TestShouldRouteToTrace_Help(t *testing.T) {
	// `rx --help` — should NOT route to trace.
	assert.False(t, shouldRouteToTrace([]string{"--help"}))
}

func TestShouldRouteToTrace_Version(t *testing.T) {
	// `rx --version` — should NOT route to trace.
	assert.False(t, shouldRouteToTrace([]string{"--version"}))
}

func TestShouldRouteToTrace_Empty(t *testing.T) {
	// No args — should NOT route to trace.
	assert.False(t, shouldRouteToTrace([]string{}))
}

func TestShouldRouteToTrace_KnownSubcommands(t *testing.T) {
	// Each known subcommand should NOT trigger trace routing.
	for _, sub := range knownSubcommands {
		assert.False(t, shouldRouteToTrace([]string{sub, "arg1"}),
			"subcommand %q should not route to trace", sub)
	}
}

func TestVersion_PrintsRXVersion(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "--version")
	require.NoError(t, err)
	assert.Contains(t, stdout, "RX")
	assert.Contains(t, stdout, version)
}

func TestHelp_ShowsAllSubcommands(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "--help")
	require.NoError(t, err)

	// The help output should mention all known subcommands.
	for _, sub := range knownSubcommands {
		assert.Contains(t, stdout, sub,
			"help output should mention subcommand %q", sub)
	}
}

func TestHelp_ShowsExamples(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "--help")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Examples:")
}

// --------------------------------------------------------------------------
// Check stub tests
// --------------------------------------------------------------------------

func TestCheck_TextOutput_PrintsNotImplemented(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "check", "(a+)+")
	require.NoError(t, err)
	assert.Contains(t, stdout, "not yet available")
}

func TestCheck_JSONOutput_ValidComplexityResponse(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "check", "--json", "(a+)+")
	require.NoError(t, err)

	var resp models.ComplexityResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err, "check --json must output valid JSON")

	assert.Equal(t, "(a+)+", resp.Regex)
	assert.Equal(t, float64(0), resp.Score)
	assert.Equal(t, "not_implemented", resp.RiskLevel)
	assert.Equal(t, "not_implemented", resp.ComplexityClass)
	assert.Equal(t, "not_implemented", resp.ComplexityNotation)
	assert.Equal(t, "unknown", resp.Level)
	assert.Equal(t, "not_implemented", resp.Risk)
	assert.NotNil(t, resp.Issues, "issues must not be nil")
	assert.Empty(t, resp.Issues, "issues must be empty")
	assert.NotNil(t, resp.Recommendations, "recommendations must not be nil")
	assert.Empty(t, resp.Recommendations, "recommendations must be empty")
	assert.NotNil(t, resp.Warnings, "warnings must not be nil")
	assert.Empty(t, resp.Warnings, "warnings must be empty")
	assert.Equal(t, len("(a+)+"), resp.PatternLength)
}

func TestCheck_JSONOutput_AllExpectedFields(t *testing.T) {
	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "check", "--json", "test")
	require.NoError(t, err)

	// Verify ALL expected top-level keys exist in the JSON.
	var raw map[string]interface{}
	err = json.Unmarshal([]byte(stdout), &raw)
	require.NoError(t, err)

	expectedKeys := []string{
		"regex", "score", "risk_level", "complexity_class", "complexity_notation",
		"issues", "recommendations", "performance", "star_height", "pattern_length",
		"has_anchors", "level", "risk", "warnings", "details", "cli_command",
	}
	for _, key := range expectedKeys {
		assert.Contains(t, raw, key, "JSON must contain field %q", key)
	}
}

func TestCheck_ExitCodeZero(t *testing.T) {
	root := NewRootCommand()
	_, _, err := executeCommand(root, "check", "abc")
	// Exit code 0 means no error returned.
	assert.NoError(t, err)
}

func TestCheck_MissingPattern_ReturnsError(t *testing.T) {
	root := NewRootCommand()
	_, _, err := executeCommand(root, "check")
	assert.Error(t, err, "check without pattern should error")
}

// --------------------------------------------------------------------------
// Format tests
// --------------------------------------------------------------------------

func TestColorize_WithColorOn(t *testing.T) {
	result := colorize("hello", ansiCyan, true)
	assert.Contains(t, result, "\033[")
	assert.Contains(t, result, ansiReset)
	assert.Contains(t, result, "hello")
}

func TestColorize_WithColorOff(t *testing.T) {
	result := colorize("hello", ansiCyan, false)
	assert.Equal(t, "hello", result)
	assert.NotContains(t, result, "\033[")
}

func TestHighlightPattern_WithColor(t *testing.T) {
	result := highlightPattern("error found here", "error", true)
	assert.Contains(t, result, "\033[")
	assert.Contains(t, result, "error")
}

func TestHighlightPattern_WithoutColor(t *testing.T) {
	result := highlightPattern("error found here", "error", false)
	assert.Equal(t, "error found here", result)
	assert.NotContains(t, result, "\033[")
}

func TestHighlightPattern_InvalidRegex(t *testing.T) {
	// Invalid regex should return the original line unchanged.
	result := highlightPattern("test line", "[invalid", true)
	assert.Equal(t, "test line", result)
}

func TestShouldColorize_NoColorFlag(t *testing.T) {
	assert.False(t, shouldColorize(true))
}

func TestShouldColorize_DefaultOn(t *testing.T) {
	// Ensure NO_COLOR is unset for this test.
	origVal := os.Getenv("NO_COLOR")
	os.Unsetenv("NO_COLOR")
	defer func() {
		if origVal != "" {
			os.Setenv("NO_COLOR", origVal)
		}
	}()

	assert.True(t, shouldColorize(false))
}

func TestShouldColorize_NOCOLOREnvVar(t *testing.T) {
	// The NO_COLOR env var should disable colors even when the flag is not set.
	origVal := os.Getenv("NO_COLOR")
	os.Setenv("NO_COLOR", "1")
	defer func() {
		if origVal == "" {
			os.Unsetenv("NO_COLOR")
		} else {
			os.Setenv("NO_COLOR", origVal)
		}
	}()

	assert.False(t, shouldColorize(false))
}

func TestFormatMatchLine_Colored(t *testing.T) {
	result := formatMatchLine("/var/log/app.log", 42, "error occurred", "error", true)
	assert.Contains(t, result, "\033[")   // Has ANSI codes.
	assert.Contains(t, result, "42")       // Line number present.
	assert.Contains(t, result, "error")    // Pattern text present.
}

func TestFormatMatchLine_NoColor(t *testing.T) {
	result := formatMatchLine("/var/log/app.log", 42, "error occurred", "error", false)
	assert.NotContains(t, result, "\033[") // No ANSI codes.
	assert.Contains(t, result, "/var/log/app.log")
	assert.Contains(t, result, "42")
	assert.Contains(t, result, "error occurred")
}

func TestFormatContextHeader_Colored(t *testing.T) {
	result := formatContextHeader("/var/log/app.log", 10, 5000, "error", true)
	assert.Contains(t, result, "\033[")
	assert.Contains(t, result, "/var/log/app.log")
	assert.Contains(t, result, "10")
	assert.Contains(t, result, "5000")
	assert.Contains(t, result, "error")
}

func TestFormatContextHeader_NoColor(t *testing.T) {
	result := formatContextHeader("/var/log/app.log", 10, 5000, "error", false)
	assert.NotContains(t, result, "\033[")
	assert.Equal(t, "=== /var/log/app.log:10:5000 [error] ===", result)
}

func TestProcessNewlineSymbol_Default(t *testing.T) {
	// Default newline symbol (\n) should not change the text.
	result := processNewlineSymbol("line1\nline2", "\n")
	assert.Equal(t, "line1\nline2", result)
}

func TestProcessNewlineSymbol_Custom(t *testing.T) {
	result := processNewlineSymbol("line1\nline2", "\\n")
	assert.Equal(t, "line1\\nline2", result)
}

func TestProcessNewlineSymbol_Empty(t *testing.T) {
	result := processNewlineSymbol("line1\nline2", "")
	assert.Equal(t, "line1\nline2", result)
}

// --------------------------------------------------------------------------
// Integration smoke tests (trace command)
// --------------------------------------------------------------------------

func TestTrace_ExplicitSubcommand_FindsMatches(t *testing.T) {
	content := "line one\nERROR something broke\nline three\nERROR again\nline five\n"
	path := createTempFile(t, "app.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "ERROR", path)
	require.NoError(t, err)

	// The output should report matches found.
	assert.Contains(t, stdout, "match")
	assert.Contains(t, stdout, "ERROR")
}

func TestTrace_JSON_ProducesValidTraceResponse(t *testing.T) {
	content := "hello world\nerror found here\ngoodbye world\n"
	path := createTempFile(t, "test.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "--json", "error", path)
	require.NoError(t, err)

	var resp models.TraceResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err, "trace --json must produce valid TraceResponse JSON")

	assert.NotEmpty(t, resp.RequestID, "request_id should be set")
	assert.NotNil(t, resp.Patterns, "patterns should not be nil")
	assert.NotNil(t, resp.Files, "files should not be nil")
	assert.NotNil(t, resp.Matches, "matches should not be nil")
	assert.NotNil(t, resp.ScannedFiles, "scanned_files should not be nil")
	assert.GreaterOrEqual(t, len(resp.Matches), 1, "should find at least 1 match")
}

func TestTrace_JSON_AllExpectedFields(t *testing.T) {
	content := "error here\n"
	path := createTempFile(t, "fields.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "--json", "error", path)
	require.NoError(t, err)

	// Verify key top-level fields exist in the JSON.
	var raw map[string]interface{}
	err = json.Unmarshal([]byte(stdout), &raw)
	require.NoError(t, err)

	requiredFields := []string{
		"request_id", "path", "time", "patterns", "files",
		"matches", "scanned_files", "skipped_files", "max_results",
	}
	for _, key := range requiredFields {
		assert.Contains(t, raw, key, "trace JSON must contain field %q", key)
	}
}

func TestTrace_MaxResults_LimitsOutput(t *testing.T) {
	// Create a file with many matches.
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "ERROR line %d\n", i)
	}
	path := createTempFile(t, "many.log", sb.String())

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "--json", "--max-results=1", "ERROR", path)
	require.NoError(t, err)

	var resp models.TraceResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(resp.Matches), 1,
		"--max-results=1 should limit to at most 1 match")
}

func TestTrace_DashDash_Passthrough(t *testing.T) {
	// Create a file with mixed-case "Error" lines. Use -- -i for case-insensitive rg search.
	content := "line one\nError here\nline three\n"
	path := createTempFile(t, "case.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "--json", "error", path, "--", "-i")
	require.NoError(t, err)

	var resp models.TraceResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	// With -i passthrough, rg should match "Error" (case-insensitive).
	assert.GreaterOrEqual(t, len(resp.Matches), 1,
		"case-insensitive passthrough via -- -i should find matches")
}

func TestTrace_NoMatches_ExitZero(t *testing.T) {
	content := "nothing interesting here\n"
	path := createTempFile(t, "empty.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "--json", "XYZNOTFOUND", path)
	// Even with no matches, exit code should be 0.
	require.NoError(t, err)

	var resp models.TraceResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)
	assert.Empty(t, resp.Matches)
}

func TestTrace_MultipleMatches_ReportsSummary(t *testing.T) {
	content := "ERROR one\nERROR two\nERROR three\n"
	path := createTempFile(t, "multi.log", content)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "trace", "ERROR", path)
	require.NoError(t, err)

	// CLI text mode should contain "matches across" summary.
	assert.Contains(t, stdout, "match")
}

// --------------------------------------------------------------------------
// Serve tests (health endpoint)
// --------------------------------------------------------------------------

func TestServe_HealthEndpoint(t *testing.T) {
	addr, shutdown, err := StartTestServer()
	require.NoError(t, err)
	defer shutdown()

	resp, err := http.Get(fmt.Sprintf("http://%s/health", addr))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var health map[string]string
	err = json.Unmarshal(body, &health)
	require.NoError(t, err)

	assert.Equal(t, "ok", health["status"])
	assert.Equal(t, version, health["version"])
}

// --------------------------------------------------------------------------
// Internal helper tests
// --------------------------------------------------------------------------

func TestIsFlag(t *testing.T) {
	assert.True(t, isFlag("--json"))
	assert.True(t, isFlag("-v"))
	assert.True(t, isFlag("--help"))
	assert.False(t, isFlag("error"))
	assert.False(t, isFlag("trace"))
	assert.False(t, isFlag(""))
}

func TestResolveContext(t *testing.T) {
	// Explicit value takes priority.
	assert.Equal(t, 5, resolveContext(5, 10, 3))
	// Symmetric value used when explicit is -1.
	assert.Equal(t, 10, resolveContext(-1, 10, 3))
	// Default used when both are -1.
	assert.Equal(t, 3, resolveContext(-1, -1, 3))
	// Zero explicit is valid (means "no context").
	assert.Equal(t, 0, resolveContext(0, 10, 3))
}

func TestJoinSearchRoots(t *testing.T) {
	result := joinSearchRoots([]string{"/var/log", "/home/data"})
	assert.Contains(t, result, "/var/log")
	assert.Contains(t, result, "/home/data")
}
