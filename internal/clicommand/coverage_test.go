package clicommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/output"
	"github.com/wlame/rx-go/internal/paths"
)

// ============================================================================
// Index command — end-to-end coverage
// ============================================================================

// TestIndexCommand_DeleteMissing verifies --delete on a nonexistent file
// produces a friendly message (no error).
func TestIndexCommand_DeleteMissing(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	f := filepath.Join(t.TempDir(), "missing.log")

	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{f, "--delete"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete on missing: got error %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "no index found") {
		t.Errorf("expected 'no index found' message: %s", buf.String())
	}
}

// TestIndexCommand_DeleteExisting builds an index, then deletes it.
// Covers runIndexDelete's happy path.
func TestIndexCommand_DeleteExisting(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "big.log")
	// Must be > threshold; use a small threshold by env so we don't
	// need to generate a 50 MB fixture.
	content := strings.Repeat("x\n", 600000) // ~1.2 MB
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var buf bytes.Buffer
	// Build.
	cmdBuild := NewIndexCommand(&buf)
	cmdBuild.SetArgs([]string{f, "--threshold=1"})
	if err := cmdBuild.Execute(); err != nil {
		t.Fatalf("build: %v", err)
	}
	// Delete.
	buf.Reset()
	cmdDel := NewIndexCommand(&buf)
	cmdDel.SetArgs([]string{f, "--delete"})
	if err := cmdDel.Execute(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(buf.String(), "deleted index") {
		t.Errorf("expected delete confirmation: %s", buf.String())
	}
}

// TestIndexCommand_BuildAndInfo covers runIndexBuild + runIndexInfo.
func TestIndexCommand_BuildAndInfo(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "data.log")
	content := strings.Repeat("hello world\n", 100000) // ~1.2 MB
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Build (text mode). Stage 9 Round 2 S3 changed the human output
	// summary to match Python's "Indexed N files in X.Xs" style — the
	// old "lines:" sub-key is now inline on the per-file line.
	var bufBuild bytes.Buffer
	cmdBuild := NewIndexCommand(&bufBuild)
	cmdBuild.SetArgs([]string{f, "--threshold=1"})
	if err := cmdBuild.Execute(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(bufBuild.String(), "index built") {
		t.Errorf("expected 'index built': %s", bufBuild.String())
	}
	if !strings.Contains(bufBuild.String(), "lines") {
		t.Errorf("expected 'lines' word in output: %s", bufBuild.String())
	}

	// Info (JSON mode).
	var bufInfo bytes.Buffer
	cmdInfo := NewIndexCommand(&bufInfo)
	cmdInfo.SetArgs([]string{f, "--info", "--json"})
	if err := cmdInfo.Execute(); err != nil {
		t.Fatalf("info: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(bufInfo.Bytes(), &data); err != nil {
		t.Fatalf("info JSON parse: %v\n%s", err, bufInfo.String())
	}
	if data["source_path"] != f {
		t.Errorf("info json source_path: got %v, want %s", data["source_path"], f)
	}
}

// TestIndexCommand_BuildBelowThreshold silently skips files smaller
// than the configured threshold (matches Python behavior — Round 1
// user decision on R1-B8). Python's CLI exits 0 with the below-threshold
// file in the `skipped` list; scripts that rely on that exit code must
// not see exit 1.
func TestIndexCommand_BuildBelowThreshold(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "small.log")
	_ = os.WriteFile(f, []byte("tiny"), 0o644)

	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{f, "--threshold=50"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("below-threshold: got error %v, want nil (silent skip)", err)
	}
}

// TestIndexCommand_JSONWrapper asserts Python's wrapper shape for
// --json output: {indexed:[...], skipped:[...], errors:[...], total_time:N}.
// Stage 9 Round 2 S3.
func TestIndexCommand_JSONWrapper(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	big := filepath.Join(root, "big.log")
	small := filepath.Join(root, "small.log")
	missing := filepath.Join(root, "nope.log")
	_ = os.WriteFile(big, []byte(strings.Repeat("x\n", 600000)), 0o644)
	_ = os.WriteFile(small, []byte("tiny"), 0o644)

	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{big, small, missing, "--threshold=1", "--json"})
	// Multiple paths — Python accepts `nargs=-1`. Below-threshold
	// (small) goes to skipped; missing goes to errors; big goes to
	// indexed.
	_ = cmd.Execute()

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"indexed", "skipped", "errors", "total_time"} {
		if _, ok := out[k]; !ok {
			t.Errorf("missing required key %q in %s", k, buf.String())
		}
	}
	// indexed must contain exactly one entry (big.log).
	indexed, _ := out["indexed"].([]any)
	if len(indexed) != 1 {
		t.Errorf("indexed: got %d entries, want 1", len(indexed))
	}
	// skipped must contain exactly one entry (small.log).
	skipped, _ := out["skipped"].([]any)
	if len(skipped) != 1 {
		t.Errorf("skipped: got %d entries, want 1", len(skipped))
	}
	// errors must contain exactly one entry (missing.log).
	errsList, _ := out["errors"].([]any)
	if len(errsList) != 1 {
		t.Errorf("errors: got %d entries, want 1", len(errsList))
	}
	// total_time must be numeric (float or int).
	if _, ok := out["total_time"].(float64); !ok {
		t.Errorf("total_time must be a number; got %T", out["total_time"])
	}
}

// TestIndexCommand_IndexedEntryShape verifies each `indexed` entry has
// the fields Python emits (path, file_type, size_bytes, created_at,
// build_time_seconds, analysis_performed, line_index, index_entries).
// Stage 9 Round 2 S3.
func TestIndexCommand_IndexedEntryShape(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "data.log")
	_ = os.WriteFile(f, []byte(strings.Repeat("hello\n", 500000)), 0o644)

	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{f, "--threshold=1", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json parse: %v", err)
	}
	indexed, _ := out["indexed"].([]any)
	if len(indexed) != 1 {
		t.Fatalf("indexed: got %d entries, want 1", len(indexed))
	}
	entry, _ := indexed[0].(map[string]any)
	// Python's key list from rx-python/src/rx/cli/index.py::_index_to_json.
	for _, k := range []string{
		"path", "file_type", "size_bytes", "created_at",
		"build_time_seconds", "analysis_performed",
		"line_index", "index_entries",
	} {
		if _, ok := entry[k]; !ok {
			t.Errorf("indexed entry missing key %q; entry=%+v", k, entry)
		}
	}
}

// TestIndexCommand_BuildDirPythonParity: an empty directory (or one with
// only below-threshold files) exits 0 with no indexed entries — matches
// Python's behavior of not treating "directory" as a hard error. Under
// Stage 9 Round 2 S3 Go now expands directories the same way.
func TestIndexCommand_BuildDirPythonParity(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	// Empty directory — no files to index, no errors.
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{dir, "--threshold=1"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("empty dir: got error %v, want nil", err)
	}
}

// TestIndexCommand_MissingSource fails with a file-not-found error.
func TestIndexCommand_MissingSource(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	var buf bytes.Buffer
	cmd := NewIndexCommand(&buf)
	cmd.SetArgs([]string{"/does/not/exist.log"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// TestIndexCommand_InfoHumanReadable covers the non-JSON info output.
func TestIndexCommand_InfoHumanReadable(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "x.log")
	_ = os.WriteFile(f, []byte(strings.Repeat("x\n", 600000)), 0o644)

	// Build first.
	var buf bytes.Buffer
	cmdBuild := NewIndexCommand(&buf)
	cmdBuild.SetArgs([]string{f, "--threshold=1"})
	_ = cmdBuild.Execute()

	// Human info.
	buf.Reset()
	cmdInfo := NewIndexCommand(&buf)
	cmdInfo.SetArgs([]string{f, "--info"})
	if err := cmdInfo.Execute(); err != nil {
		t.Fatalf("info: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"Index for:", "file_type:", "size_bytes:", "index_entries:"} {
		if !strings.Contains(got, want) {
			t.Errorf("info missing %q in output: %s", want, got)
		}
	}
}

// ============================================================================
// Compress command — JSON wrapper coverage (Stage 9 Round 2 S4)
// ============================================================================

// TestCompressCommand_JSONWrapper asserts Python's wrapper shape for
// --json output: {"files": [{input, action, success, output, ...}]}.
func TestCompressCommand_JSONWrapper(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "data.log")
	// Small input is enough — we only need the JSON envelope shape.
	_ = os.WriteFile(f, []byte(strings.Repeat("hello\n", 100)), 0o644)

	var buf bytes.Buffer
	cmd := NewCompressCommand(&buf)
	cmd.SetArgs([]string{f, "--json", "--level=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compress --json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	files, ok := out["files"].([]any)
	if !ok {
		t.Fatalf("expected top-level `files` array in %s", buf.String())
	}
	if len(files) != 1 {
		t.Fatalf("files: got %d entries, want 1", len(files))
	}
	entry, _ := files[0].(map[string]any)
	// Python's key set (from rx-python/src/rx/cli/compress.py::_compress_file).
	for _, k := range []string{
		"input", "action", "success", "output",
		"compressed_size", "decompressed_size", "frame_count",
		"compression_ratio",
	} {
		if _, ok := entry[k]; !ok {
			t.Errorf("compress entry missing key %q; entry=%+v", k, entry)
		}
	}
	if entry["action"] != "compress" {
		t.Errorf("action: got %v, want \"compress\"", entry["action"])
	}
	if entry["success"] != true {
		t.Errorf("success: got %v, want true", entry["success"])
	}
}

// ============================================================================
// Samples command — broader coverage
// ============================================================================

// TestSamplesCommand_OffsetsRejected — offsets are parsed but currently
// fall back to the line-sampling path; ensure the command at least runs
// when both flags are legal.
func TestSamplesCommand_RangeMode(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "row %d\n", i)
	}
	_ = os.WriteFile(f, []byte(sb.String()), 0o644)

	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=3-5"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"row 3", "row 4", "row 5"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in range output: %s", want, got)
		}
	}
}

// TestSamplesCommand_NegativeLine — -1 selects the last line.
func TestSamplesCommand_NegativeLine(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644)
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=-1", "--context=0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "three") {
		t.Errorf("negative index should return last line, got: %s", buf.String())
	}
}

// TestSamplesCommand_JSONOutput covers the JSON format path.
func TestSamplesCommand_JSONOutput(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("a\nb\nc\n"), 0o644)
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=2", "--context=0", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	if data["path"] != f {
		t.Errorf("json path mismatch: got %v, want %s", data["path"], f)
	}
}

// TestSamplesCommand_MissingFile returns file-not-found.
func TestSamplesCommand_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{"/does/not/exist.log", "--lines=1"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// TestSamplesCommand_Directory returns usage-error.
func TestSamplesCommand_Directory(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{dir, "--lines=1"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected error for directory input")
	}
}

// TestSamplesCommand_ColoredOutput_Always forces color via --color=always
// and verifies ANSI codes appear.
func TestSamplesCommand_ColoredOutput_Always(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\n"), 0o644)
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=2", "--context=0", "--color=always"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), output.ColorBrightCyan) {
		t.Errorf("expected ANSI codes in --color=always output")
	}
}

// TestSamplesCommand_ColoredOutput_Never suppresses colors.
func TestSamplesCommand_ColoredOutput_Never(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\n"), 0o644)
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=1", "--context=0", "--color=never"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(buf.String(), output.ColorBrightCyan) {
		t.Errorf("--color=never should not emit ANSI codes")
	}
}

// TestSamplesCommand_ByteOffsetMode_ResolvesToLineContainingByte
// covers Stage 9 Round 2 R1-B4: the CLI's --offsets (or -b) flag must
// dispatch to BYTE-offset mode, not treat the value as a line number.
// Python's `rx samples file -b 5000` goes to the line containing byte
// 5000; Round 1 Go incorrectly treated 5000 as a line index.
//
// Deterministic fixture: 20 lines of "line NNN\n" (9 bytes each).
// Byte offset 85 falls inside line 10 (line 10 starts at byte 81,
// ends at byte 89).
func TestSamplesCommand_ByteOffsetMode_ResolvesToLineContainingByte(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %03d\n", i)
	}
	_ = os.WriteFile(f, []byte(sb.String()), 0o644)

	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-b", "85", "-c", "0", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	offsets, _ := data["offsets"].(map[string]any)
	// Offset 85 must map to line 10 (NOT 85, which would indicate
	// line-number interpretation).
	got, ok := offsets["85"].(float64)
	if !ok {
		t.Fatalf("offsets[\"85\"] missing or wrong type: %v", offsets)
	}
	if got != 10 {
		t.Errorf("offsets[\"85\"] = %v, want 10 (line containing byte 85); Round 1 bug was that this returned 85",
			got)
	}
	// Sample content should be the single line.
	samples, _ := data["samples"].(map[string]any)
	samArr, _ := samples["85"].([]any)
	if len(samArr) != 1 || samArr[0] != "line 010" {
		t.Errorf("sample = %v, want ['line 010']", samArr)
	}
}

// TestSamplesCommand_MultiRangeSpec covers Stage 9 Round 2 R1-B4
// user design: a single --lines spec can contain multiple ranges
// (comma-separated), each with its own sample slice.
func TestSamplesCommand_MultiRangeSpec(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %03d\n", i)
	}
	_ = os.WriteFile(f, []byte(sb.String()), 0o644)

	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-l", "2-5,10-12", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("json parse: %v\n%s", err, buf.String())
	}
	samples, _ := data["samples"].(map[string]any)
	for _, key := range []string{"2-5", "10-12"} {
		arr, _ := samples[key].([]any)
		if len(arr) == 0 {
			t.Errorf("multi-range: missing samples for %q; got %v", key, samples)
		}
	}
	// Spot-check: 2-5 should have 4 lines.
	arr, _ := samples["2-5"].([]any)
	if len(arr) != 4 {
		t.Errorf("samples[\"2-5\"]: got %d lines, want 4", len(arr))
	}
}

// TestSamplesCommand_PythonShortFlags covers Stage 9 Round 2 R1-B11:
// Python's samples CLI uses `-b / -l / -c / --no-color / -r` as short
// aliases. Python scripts migrating to rx-go must continue to work,
// so the Go implementation registers the same short aliases.
func TestSamplesCommand_PythonShortFlags(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644)

	// -l short flag for --lines.
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-l", "3", "-c", "0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("-l short flag: %v", err)
	}
	if !strings.Contains(buf.String(), "three") {
		t.Errorf("-l 3 -c 0 should output 'three', got: %s", buf.String())
	}

	// -b short flag for --offsets: verify flag is recognized.
	// (Byte-offset → line resolution is Sub-agent U's work — this test
	// only asserts the flag parses and the command executes.)
	buf.Reset()
	cmd = NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-b", "0", "-c", "0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("-b short flag: %v", err)
	}
	// If the flag were unrecognized, cobra would error before
	// Execute(). Executing successfully is enough evidence.

	// --no-color should be accepted and suppress color codes.
	buf.Reset()
	cmd = NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-l", "1", "-c", "0", "--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--no-color: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("--no-color should suppress ANSI codes, got: %s", buf.String())
	}

	// -r short flag for --regex (highlight). Paired with --color=always
	// to force color output so the highlight escape is observable.
	buf.Reset()
	cmd = NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "-l", "1", "-c", "0", "-r", "one", "--color=always"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("-r short flag: %v", err)
	}
	if !strings.Contains(buf.String(), output.ColorBrightRed) {
		t.Errorf("-r one --color=always should highlight, got: %q", buf.String())
	}
}

// TestSamplesCommand_RegexHighlight activates the --regex highlighter.
func TestSamplesCommand_RegexHighlight(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("one\nerror occurred\nthree\n"), 0o644)
	var buf bytes.Buffer
	cmd := NewSamplesCommand(&buf)
	cmd.SetArgs([]string{f, "--lines=2", "--context=0", "--regex=error", "--color=always"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), output.ColorBrightRed) {
		t.Errorf("expected red highlight escape in output: %q", buf.String())
	}
}

// TestShouldColorize exercises all three branches of the flag-driven
// color resolver.
func TestShouldColorize(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("RX_NO_COLOR", "")
	var buf bytes.Buffer
	if !shouldColorize("always", &buf) {
		t.Errorf("always should return true even for buffer writer")
	}
	if shouldColorize("never", &buf) {
		t.Errorf("never should return false")
	}
	// Auto on a buffer should default to the colorDecision result
	// (which is true for non-*os.File).
	if !shouldColorize("", &buf) {
		t.Errorf("auto on bytes.Buffer should return true")
	}
	t.Setenv("NO_COLOR", "1")
	if shouldColorize("", &buf) {
		t.Errorf("auto should respect NO_COLOR")
	}
}

// ============================================================================
// Trace command — runTrace direct coverage
// ============================================================================

// TestTraceCommand_HumanOutput exercises the non-JSON branch of runTrace.
func TestTraceCommand_HumanOutput(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("hello\nerror one\nok\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", f})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[error]") {
		t.Errorf("human trace output missing pattern label: %s", out)
	}
	if !strings.Contains(out, "error one") {
		t.Errorf("human trace output missing match: %s", out)
	}
	if !strings.Contains(out, "matches in") {
		t.Errorf("human trace output missing summary: %s", out)
	}
}

// TestTraceCommand_NoMatches — "no matches" branch of writeTraceHuman.
func TestTraceCommand_NoMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("nothing matches here\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"zzzzzzzz", f})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "no matches in") {
		t.Errorf("empty-result branch missing summary: %s", buf.String())
	}
}

// TestTraceCommand_NoPattern returns a usage error.
func TestTraceCommand_NoPattern(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	// No positional, no --regexp; cwd has no files or all non-matching.
	// runTrace should emit a usage error.
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Errorf("expected error on empty invocation")
	}
}

// TestTraceCommand_SandboxViolation verifies paths outside configured
// search roots are rejected with a non-nil error.
func TestTraceCommand_SandboxViolation(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	root := t.TempDir()
	// Force a sandbox via paths.SetSearchRoots.
	otherDir := t.TempDir()
	if err := paths.SetSearchRoots([]string{root}); err != nil {
		t.Fatalf("set roots: %v", err)
	}
	t.Cleanup(paths.Reset)

	f := filepath.Join(otherDir, "a.log")
	_ = os.WriteFile(f, []byte("error\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", f})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected sandbox error for path outside root")
	}
}

// TestTraceCommand_StdinReject returns an error for '-' input.
func TestTraceCommand_StdinReject(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", "-"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("expected error for stdin input")
	}
}

// TestTraceCommand_DirectoryRecursiveByDefault covers Stage 9 Round 2
// S5 + R1-B7: `rx trace <dir>` recurses by default (Python parity).
// Round 1 Go only scanned the top-level directory; subdirectories were
// skipped silently.
//
// Test setup: dir/a.log + dir/sub/b.log both contain matches. Default
// invocation (no flag) must find BOTH files.
func TestTraceCommand_DirectoryRecursiveByDefault(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	topLog := filepath.Join(root, "a.log")
	_ = os.WriteFile(topLog, []byte("error one\n"), 0o644)
	subDir := filepath.Join(root, "sub")
	_ = os.MkdirAll(subDir, 0o755)
	subLog := filepath.Join(subDir, "b.log")
	_ = os.WriteFile(subLog, []byte("error two\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "error one") {
		t.Errorf("recursive default: missing match from top-level a.log: %s", out)
	}
	if !strings.Contains(out, "error two") {
		t.Errorf("recursive default: missing match from sub/b.log — default must recurse: %s", out)
	}
}

// TestTraceCommand_NoRecursiveFlag covers the opt-out path. With
// --no-recursive the scan stops at the top level (Python has no exact
// equivalent; Go-only escape hatch).
func TestTraceCommand_NoRecursiveFlag(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	topLog := filepath.Join(root, "a.log")
	_ = os.WriteFile(topLog, []byte("error one\n"), 0o644)
	subDir := filepath.Join(root, "sub")
	_ = os.MkdirAll(subDir, 0o755)
	subLog := filepath.Join(subDir, "b.log")
	_ = os.WriteFile(subLog, []byte("error two\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", root, "--no-recursive"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "error one") {
		t.Errorf("--no-recursive: top-level match still expected: %s", out)
	}
	if strings.Contains(out, "error two") {
		t.Errorf("--no-recursive: sub-dir match should NOT appear, got: %s", out)
	}
}

// TestTraceCommand_SingleFileUnaffected regression guard: passing a
// single file (not a directory) must behave exactly as before — no
// surprising recursive-style walking.
func TestTraceCommand_SingleFileUnaffected(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	f := filepath.Join(root, "a.log")
	_ = os.WriteFile(f, []byte("error one\ngood\nerror two\n"), 0o644)

	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", f})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "error one") || !strings.Contains(out, "error two") {
		t.Errorf("single-file scan missing expected matches: %s", out)
	}
}

// TestTraceCommand_NonexistentFileExitsNonzero covers Stage 9 Round 2
// R1-B6: Python exits 1 with "Path not found" on stderr; Go must match
// (was silently reporting "no matches in 0 files" and exit 0 in Round 1).
func TestTraceCommand_NonexistentFileExitsNonzero(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SetArgs([]string{"error", "/tmp/does-not-exist-rx-parity.log"})
	if err := cmd.Execute(); err == nil {
		t.Errorf("nonexistent file: want error, got nil (Python parity)")
	}
}

// TestResolveBeforeAfter exercises the precedence rules.
func TestResolveBeforeAfter(t *testing.T) {
	cases := []struct {
		name         string
		p            traceParams
		wantB, wantA int
	}{
		{"default", traceParams{}, 0, 0},
		{"context only", traceParams{ctxLines: 3}, 3, 3},
		{"before overrides", traceParams{ctxLines: 3, beforeCtx: 5}, 5, 3},
		{"after overrides", traceParams{ctxLines: 3, afterCtx: 7}, 3, 7},
		{"both override", traceParams{ctxLines: 3, beforeCtx: 5, afterCtx: 7}, 5, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBefore(tc.p); got != tc.wantB {
				t.Errorf("resolveBefore: got %d, want %d", got, tc.wantB)
			}
			if got := resolveAfter(tc.p); got != tc.wantA {
				t.Errorf("resolveAfter: got %d, want %d", got, tc.wantA)
			}
		})
	}
}

// ============================================================================
// Serve command — smoke-level coverage
// ============================================================================

// TestServeCommand_Construction just exercises NewServeCommand's flag
// registration; running the server is heavier and covered via
// integration tests in internal/webapi.
func TestServeCommand_Construction(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewServeCommand(&buf, "test-version")
	if cmd.Use == "" {
		t.Error("NewServeCommand should set Use")
	}
	// Flag presence spot-check.
	for _, flag := range []string{"port", "host", "search-root", "skip-frontend"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("serve is missing flag: %s", flag)
		}
	}
}

// ============================================================================
// Common helpers
// ============================================================================

// TestStdinIsPipe covers the happy branch (os.Stdin is usually a pipe
// during test execution). The error branch (no way to stat) is not
// reachable in normal conditions.
func TestStdinIsPipe(t *testing.T) {
	// Just exercise the function — don't assert a specific value
	// because the test runner may leave stdin as TTY or pipe
	// depending on environment.
	_ = stdinIsPipe()
}

// Stage 9 Round 2 U rework: parseRangeOrSingle was retired in favor of
// internal/samples.ParseCSV / parseOne. Equivalent test coverage lives
// in internal/samples/parser_test.go. This stub is kept only to
// acknowledge the rename and document the relocation.

// TestWriteTraceJSON covers the JSON encoder helper.
func TestWriteTraceJSON(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{
		"matches": []any{map[string]any{"file": "f1", "offset": 100}},
		"time":    0.5,
	}
	if err := writeTraceJSON(&buf, payload); err != nil {
		t.Fatalf("writeTraceJSON: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if _, ok := back["matches"]; !ok {
		t.Errorf("encoded payload missing 'matches' key: %s", buf.String())
	}
	// Pretty-printed: must contain indentation whitespace.
	if !strings.Contains(buf.String(), "  ") {
		t.Errorf("expected indented JSON output, got: %s", buf.String())
	}
}

// TestWriteTraceJSON_EncodeError covers the error path (unencodable value).
func TestWriteTraceJSON_EncodeError(t *testing.T) {
	var buf bytes.Buffer
	err := writeTraceJSON(&buf, map[string]any{
		"bad": make(chan int), // channels aren't JSON-encodable
	})
	if err == nil {
		t.Errorf("expected encode error for non-JSON value")
	}
}

// Stage 9 Round 2 U rework: splitLinesWithLengths / rawLine were
// retired when the samples CLI moved to the shared internal/samples
// resolver. The equivalent logic (line offsets + newline handling) is
// exercised end-to-end in internal/samples/resolver_test.go, and the
// CLI path is covered by the higher-level TestSamplesCommand_* tests.
