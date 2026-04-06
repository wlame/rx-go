package cli

import (
	"os"
	"path/filepath"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wlame/rx/internal/models"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// makeSamplesTestFile creates a temp file with known numbered lines and returns
// its path along with the byte offset of each line start.
func makeSamplesTestFile(t *testing.T, lineCount int) (string, []int64) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "samples_test.log")

	var content []byte
	var offsets []int64
	for i := 1; i <= lineCount; i++ {
		offsets = append(offsets, int64(len(content)))
		line := []byte("Line " + itoa(i) + ": content for line number " + itoa(i) + "\n")
		content = append(content, line...)
	}

	require.NoError(t, os.WriteFile(path, content, 0644))
	return path, offsets
}

// itoa is a simple int-to-string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// --------------------------------------------------------------------------
// Basic byte offset tests
// --------------------------------------------------------------------------

func TestSamples_ByteOffset_BasicOffset(t *testing.T) {
	// rx samples FILE -b 0 should return context lines around offset 0 (line 1).
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-b", "0")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 1:")
	assert.Contains(t, stdout, "Samples from")
}

func TestSamples_ByteOffset_MiddleOfFile(t *testing.T) {
	// Targeting offset of line 5 should include that line in the output.
	path, offsets := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-b", itoa(int(offsets[4])))
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 5:")
}

func TestSamples_ByteOffset_MultipleOffsets(t *testing.T) {
	// Providing two byte offsets should return samples for both.
	path, offsets := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-b", itoa(int(offsets[1])), "-b", itoa(int(offsets[8])))
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 2:")
	assert.Contains(t, stdout, "Line 9:")
}

// --------------------------------------------------------------------------
// Basic line number tests
// --------------------------------------------------------------------------

func TestSamples_LineNumber_BasicLine(t *testing.T) {
	// rx samples FILE -l 5 should return context around line 5.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "5")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 5:")
}

func TestSamples_LineNumber_FirstLine(t *testing.T) {
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "1")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 1:")
}

func TestSamples_LineNumber_LastLine(t *testing.T) {
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "10")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 10:")
}

func TestSamples_LineNumber_MultipleLines(t *testing.T) {
	// rx samples FILE -l 3 -l 8 should return samples for both lines.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "3", "-l", "8")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 3:")
	assert.Contains(t, stdout, "Line 8:")
}

func TestSamples_LineNumber_CommaList(t *testing.T) {
	// cobra IntSlice supports comma-separated values: -l 1,5,10
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "1,5,10")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Line 1:")
	assert.Contains(t, stdout, "Line 5:")
	assert.Contains(t, stdout, "Line 10:")
}

// --------------------------------------------------------------------------
// JSON output tests
// --------------------------------------------------------------------------

func TestSamples_ByteOffset_JSON(t *testing.T) {
	// rx samples FILE -b 0 --json should return valid SamplesResponse JSON.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-b", "0", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err, "JSON output must be valid SamplesResponse")

	assert.Equal(t, path, resp.Path)
	assert.Equal(t, 3, resp.BeforeCtx, "default before context should be 3")
	assert.Equal(t, 3, resp.AfterCtx, "default after context should be 3")
	assert.Contains(t, resp.Offsets, "0")
	assert.Contains(t, resp.Samples, "0")
	assert.NotEmpty(t, resp.Samples["0"], "samples at offset 0 should not be empty")
}

func TestSamples_ByteOffset_JSON_AllExpectedKeys(t *testing.T) {
	// Verify all expected top-level fields exist in the JSON output.
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-b", "0", "--json")
	require.NoError(t, err)

	var raw map[string]interface{}
	err = json.Unmarshal([]byte(stdout), &raw)
	require.NoError(t, err)

	expectedKeys := []string{
		"path", "offsets", "lines", "before_context", "after_context",
		"samples", "is_compressed", "compression_format", "cli_command",
	}
	for _, key := range expectedKeys {
		assert.Contains(t, raw, key, "JSON must contain field %q", key)
	}
}

func TestSamples_LineNumber_JSON(t *testing.T) {
	// rx samples FILE -l 3 --json should return valid JSON with line-keyed samples.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "3", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Equal(t, path, resp.Path)
	assert.Contains(t, resp.Lines, "3", "lines map should contain key '3'")
	assert.Empty(t, resp.Offsets, "offsets should be empty when using -l")
	assert.Contains(t, resp.Samples, "3")

	// The sample should contain the target line text.
	sampleLines := resp.Samples["3"]
	found := false
	for _, line := range sampleLines {
		if contains(line, "Line 3:") {
			found = true
			break
		}
	}
	assert.True(t, found, "sample at line 3 should contain 'Line 3:'")
}

func TestSamples_LineNumber_JSON_MultipleLines(t *testing.T) {
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "3", "-l", "8", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Contains(t, resp.Samples, "3")
	assert.Contains(t, resp.Samples, "8")
	assert.Len(t, resp.Samples, 2)
}

// --------------------------------------------------------------------------
// Context control tests
// --------------------------------------------------------------------------

func TestSamples_ContextControl_BeforeAfter(t *testing.T) {
	// -B 2 -A 1 gives 2 lines before and 1 line after the target.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "5", "-B", "2", "-A", "1", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Equal(t, 2, resp.BeforeCtx, "before_context should be 2")
	assert.Equal(t, 1, resp.AfterCtx, "after_context should be 1")

	// With B=2, A=1 around line 5: should get lines 3, 4, 5, 6
	sampleLines := resp.Samples["5"]
	assert.Len(t, sampleLines, 4, "should have 4 lines: 2 before + target + 1 after")
	assert.Contains(t, sampleLines[0], "Line 3:")
	assert.Contains(t, sampleLines[1], "Line 4:")
	assert.Contains(t, sampleLines[2], "Line 5:")
	assert.Contains(t, sampleLines[3], "Line 6:")
}

func TestSamples_ContextControl_Symmetric(t *testing.T) {
	// -c 2 gives 2 lines on both sides.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "5", "-c", "2", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Equal(t, 2, resp.BeforeCtx)
	assert.Equal(t, 2, resp.AfterCtx)

	sampleLines := resp.Samples["5"]
	assert.Len(t, sampleLines, 5, "should have 5 lines: 2 before + target + 2 after")
}

func TestSamples_ContextControl_Zero(t *testing.T) {
	// -c 0 returns only the target line with no context.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "5", "-c", "0", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Equal(t, 0, resp.BeforeCtx)
	assert.Equal(t, 0, resp.AfterCtx)

	sampleLines := resp.Samples["5"]
	assert.Len(t, sampleLines, 1, "with -c 0, should return exactly 1 line")
	assert.Contains(t, sampleLines[0], "Line 5:")
}

func TestSamples_ContextControl_Default(t *testing.T) {
	// Without any context flags, default should be 3.
	path, _ := makeSamplesTestFile(t, 20)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "10", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	assert.Equal(t, 3, resp.BeforeCtx, "default before_context should be 3")
	assert.Equal(t, 3, resp.AfterCtx, "default after_context should be 3")

	// Lines 7-13 = 7 total (3 before + target + 3 after)
	sampleLines := resp.Samples["10"]
	assert.Len(t, sampleLines, 7)
}

func TestSamples_ContextControl_CLIOutput(t *testing.T) {
	// Human-readable output should include the context info line.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "5", "-c", "2")
	require.NoError(t, err)

	assert.Contains(t, stdout, "context: 2 before, 2 after")
}

// --------------------------------------------------------------------------
// Error cases
// --------------------------------------------------------------------------

func TestSamples_Error_MissingFile(t *testing.T) {
	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", "/nonexistent/path/file.txt", "-b", "0")
	assert.Error(t, err, "should error on missing file")
}

func TestSamples_Error_NoOffsetOrLine(t *testing.T) {
	// Neither -b nor -l provided should return an error.
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", path)
	assert.Error(t, err, "should error when no offset or line flag is provided")
	assert.Contains(t, err.Error(), "must provide")
}

func TestSamples_Error_BothOffsetAndLine(t *testing.T) {
	// Using both -b and -l should return an error.
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", path, "-b", "0", "-l", "1")
	assert.Error(t, err, "should error when both -b and -l are used")
	assert.Contains(t, err.Error(), "cannot use both")
}

func TestSamples_Error_LineBeyondFile(t *testing.T) {
	// Line number beyond file length should return an error.
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", path, "-l", "999")
	assert.Error(t, err, "line beyond file length should return error")
	assert.Contains(t, err.Error(), "out of bounds")
}

func TestSamples_Error_LineZero(t *testing.T) {
	// Line 0 is invalid (1-based).
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", path, "-l", "0")
	assert.Error(t, err, "line 0 should return error")
}

func TestSamples_Error_MissingPath(t *testing.T) {
	// No path argument at all should fail.
	root := NewRootCommand()
	_, _, err := executeCommand(root, "samples", "-b", "0")
	// cobra should error because it expects exactly 1 arg (the path).
	assert.Error(t, err)
}

// --------------------------------------------------------------------------
// Edge cases
// --------------------------------------------------------------------------

func TestSamples_Edge_SingleLineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.txt")
	require.NoError(t, os.WriteFile(path, []byte("Only one line here\n"), 0644))

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path, "-l", "1")
	require.NoError(t, err)

	assert.Contains(t, stdout, "Only one line")
}

func TestSamples_Edge_LargeContextOnSmallFile(t *testing.T) {
	// Requesting more context than the file has should clamp gracefully.
	path, _ := makeSamplesTestFile(t, 3)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "2", "-c", "100", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	// Should get all 3 lines even though context is 100.
	sampleLines := resp.Samples["2"]
	assert.Len(t, sampleLines, 3)
}

func TestSamples_Edge_ByteOffsetReturnsSurroundingLines(t *testing.T) {
	// Verify byte offset mode actually returns surrounding context.
	path, offsets := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-b", itoa(int(offsets[4])), "-c", "1", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	key := itoa(int(offsets[4]))
	sampleLines := resp.Samples[key]
	// With -c 1 around line 5: should get lines 4, 5, 6
	assert.Len(t, sampleLines, 3)
	assert.Contains(t, sampleLines[0], "Line 4:")
	assert.Contains(t, sampleLines[1], "Line 5:")
	assert.Contains(t, sampleLines[2], "Line 6:")
}

func TestSamples_Edge_ContextClampedAtStart(t *testing.T) {
	// Context around line 1 with before=5 should clamp at the start.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "1", "-B", "5", "-A", "2", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	// Line 1 with before=5 can only get line 1 + 2 after = 3 lines.
	sampleLines := resp.Samples["1"]
	assert.Len(t, sampleLines, 3)
	assert.Contains(t, sampleLines[0], "Line 1:")
}

func TestSamples_Edge_ContextClampedAtEnd(t *testing.T) {
	// Context around last line with after=5 should clamp at the end.
	path, _ := makeSamplesTestFile(t, 10)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-l", "10", "-B", "2", "-A", "5", "--json")
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	// Line 10 with after=5 can only get 2 before + target = 3 lines.
	sampleLines := resp.Samples["10"]
	assert.Len(t, sampleLines, 3)
	assert.Contains(t, sampleLines[2], "Line 10:")
}

func TestSamples_Edge_ByteOffsetBeyondFile(t *testing.T) {
	// Byte offset beyond file size should return no samples (empty list).
	path, _ := makeSamplesTestFile(t, 5)

	root := NewRootCommand()
	stdout, _, err := executeCommand(root, "samples", path,
		"-b", "999999", "--json")
	// GetContext returns nil,nil for out-of-bounds offset.
	// The command writes the JSON but the samples list is empty.
	require.NoError(t, err)

	var resp models.SamplesResponse
	err = json.Unmarshal([]byte(stdout), &resp)
	require.NoError(t, err)

	// Offset 999999 should have an empty sample.
	sampleLines := resp.Samples["999999"]
	assert.Nil(t, sampleLines, "out-of-bounds byte offset should have nil samples")
}

// --------------------------------------------------------------------------
// Helpers for string matching
// --------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
