package clicommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// traceSignatureFile is the user signature file of the tests below,
// beside the built-in signatures: an error line, an opt-in block from
// BEGIN to END, a redacting private-key block and an opt-in pii regex.
const traceSignatureFile = `signatures:
  - id: error-line
    name: Error line
    group: severity
    description: A line that says ERROR.
    regex: '\bERROR\b'
  - id: big-block
    name: Big block
    group: structure
    level: low
    description: A block from BEGIN to END.
    default: false
    block:
      start: '^BEGIN'
      end: '^END'
      max_lines: 1000
  - id: pem-key
    name: Private key
    group: secret
    level: high
    description: A PEM private key.
    redact: true
    block:
      start: '-----BEGIN [A-Z ]*PRIVATE KEY-----'
      end: '-----END [A-Z ]*PRIVATE KEY-----'
  - id: email-address
    name: Email address
    group: pii
    description: An email address.
    default: false
    regex: '[a-z]+@[a-z]+\.com'
`

// withTraceSignatures points the signature search path at
// traceSignatureFile and the cache at a fresh directory, and returns
// the cache directory.
func withTraceSignatures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yml")
	if err := os.WriteFile(path, []byte(traceSignatureFile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RX_SIGNATURES_PATH", path)
	cache := t.TempDir()
	t.Setenv("RX_CACHE_DIR", cache)
	return cache
}

// runTraceCmd runs `rx trace ARGS` in process and returns what it
// printed and its error.
func runTraceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewTraceCommand(&buf)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// traceSignaturesJSON runs `rx trace ARGS --json` and parses the answer;
// any error fails the test.
func traceSignaturesJSON(t *testing.T, args ...string) rxtypes.TraceResponse {
	t.Helper()
	out, err := runTraceCmd(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("rx trace %v: %v", args, err)
	}
	var resp rxtypes.TraceResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse --json: %v\n%s", err, out)
	}
	return resp
}

// writeText writes text to name under dir and returns the path.
func writeText(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tracebackLog holds a Python traceback, two error lines, a timeout and
// an email address.
const tracebackLog = "LINE 1 start\n" +
	"LINE 2 ERROR boom one\n" +
	"Traceback (most recent call last):\n" +
	"  File \"a.py\", line 1, in <module>\n" +
	"ValueError: bad input\n" +
	"LINE 6 request timeout after 30s\n" +
	"LINE 7 ERROR boom two mail bob@example.com\n"

// findingKeys lists each finding as file:signature:start-end.
func findingKeys(resp rxtypes.TraceResponse) []string {
	var out []string
	for _, f := range resp.Findings {
		out = append(out, fmt.Sprintf("%s:%s:%d-%d", f.File, f.Signature, f.StartLine, f.EndLine))
	}
	return out
}

// A bare --signatures scans with the default set, and a positional is a
// path: the answer has the selection, the findings with their lines,
// and no pattern and no match.
func TestTraceSignatures_BareFlagScansTheDefaultSet(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	resp := traceSignaturesJSON(t, "--signatures", path)
	wantSel := []string{"aws-access-key", "blank-run", "error-line", "pem-key", "python-traceback"}
	if got := slices.Sorted(maps.Keys(resp.Signatures)); !slices.Equal(got, wantSel) {
		t.Errorf("signatures %v, want the default set %v", got, wantSel)
	}
	want := []string{"f1:error-line:2-2", "f1:python-traceback:3-5", "f1:error-line:7-7"}
	if got := findingKeys(resp); !slices.Equal(got, want) {
		t.Errorf("findings %v, want %v", got, want)
	}
	if len(resp.Patterns) != 0 || len(resp.Matches) != 0 || resp.Files["f1"] != path {
		t.Errorf("patterns %v matches %v files %v", resp.Patterns, resp.Matches, resp.Files)
	}
	if got := resp.Findings[1].Lines; len(got) != 3 || got[2] != "ValueError: bad input" {
		t.Errorf("traceback lines %q", got)
	}
}

// rx trace --signatures=traceback,sql scans every text file under the
// paths, each with its own file id, and lists a file that is not text
// as skipped with the reason, in --json and in the human output.
func TestTraceSignatures_ScansEveryTextFileAndListsTheSkippedOnes(t *testing.T) {
	withTraceSignatures(t)
	root := t.TempDir()
	app := writeText(t, root, "app.log", tracebackLog)
	dir := filepath.Join(root, "dir")
	a := writeText(t, dir, "a.log", "Traceback (most recent call last):\n  File \"b.py\", line 2\nKeyError: 'x'\n")
	writeText(t, dir, "b.log", "nothing here\n")
	binary := writeText(t, dir, "c.bin", "ERROR\x00binary\n")
	resp := traceSignaturesJSON(t, "--signatures=traceback,sql", app, dir)
	if got := slices.Sorted(maps.Keys(resp.Signatures)); !slices.Equal(got, []string{"python-traceback"}) {
		t.Errorf("signatures %v", got)
	}
	if resp.Files["f1"] != app || resp.Files["f2"] != a || len(resp.FindingSummary) != 3 {
		t.Errorf("files %v summary %v", resp.Files, resp.FindingSummary)
	}
	if got := findingKeys(resp); !slices.Equal(got, []string{"f1:python-traceback:3-5", "f2:python-traceback:1-3"}) {
		t.Errorf("findings %v", got)
	}
	if len(resp.SkipReasons) != 1 || resp.SkipReasons[0].Path != binary || !strings.Contains(resp.SkipReasons[0].Reason, "not a text file") {
		t.Errorf("skip reasons %+v", resp.SkipReasons)
	}
	human, err := runTraceCmd(t, "--signatures=traceback,sql", "--no-color", app, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human, "Files skipped:") || !strings.Contains(human, binary+": not a text file") {
		t.Errorf("human output lacks the skipped file:\n%s", human)
	}
}

// With --signatures a pattern goes with -e: the answer has its matches
// and the findings of the selection (all but the pii group here).
func TestTraceSignatures_PatternWithEGivesMatchesAndFindings(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog+"BEGIN\nx\nEND\n")
	resp := traceSignaturesJSON(t, "--signatures=all,-pii", "-e", "timeout", path)
	if len(resp.Matches) != 1 || resp.Matches[0].AbsoluteLineNumber != 6 || resp.Patterns["p1"] != "timeout" {
		t.Errorf("matches %+v patterns %v", resp.Matches, resp.Patterns)
	}
	if _, ok := resp.Signatures["email-address"]; ok {
		t.Error("the pii group was removed from the selection")
	}
	if got := findingKeys(resp); !slices.Contains(got, "f1:big-block:8-10") || !slices.Contains(got, "f1:python-traceback:3-5") {
		t.Errorf("findings %v", got)
	}
}

// With --signatures every positional is a path: a word that looks like
// a pattern is a missing file (exit 3, naming it). Without the flag the
// first positional is still the pattern.
func TestTraceSignatures_PositionalsArePaths(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	_, err := runTraceCmd(t, "--signatures=default", "ERROR", path)
	if code := exitCode(err); code != ExitFileNotFound || !strings.Contains(err.Error(), "ERROR") {
		t.Errorf("exit %d (%v), want %d naming ERROR", code, err, ExitFileNotFound)
	}
	resp := traceSignaturesJSON(t, "ERROR", path)
	if len(resp.Matches) != 2 || resp.Signatures != nil {
		t.Errorf("without --signatures: matches %d signatures %v", len(resp.Matches), resp.Signatures)
	}
}

// A selection that cannot be resolved is a usage error, exit 2, before
// any file is read.
func TestTraceSignatures_SelectionErrorExitsTwo(t *testing.T) {
	withTraceSignatures(t)
	for _, sel := range []string{"--signatures=nope", "--signatures=tracebak"} {
		_, err := runTraceCmd(t, sel, filepath.Join(t.TempDir(), "missing.log"))
		if code := exitCode(err); code != ExitUsageError {
			t.Errorf("%s: exit %d (%v), want 2", sel, code, err)
		}
	}
	_, err := runTraceCmd(t, "--signatures=tracebak", filepath.Join(t.TempDir(), "missing.log"))
	if err == nil || !strings.Contains(err.Error(), "close names: traceback") {
		t.Errorf("error %v, want the close names", err)
	}
}

// The JSON of a trace with signatures: every finding field under its
// name and type, the selection's description, the summary per file,
// and the findings in the one order.
func TestTraceSignatures_JSONFieldsTypesAndOrder(t *testing.T) {
	withTraceSignatures(t)
	dir := t.TempDir()
	b := writeText(t, dir, "b.log", "ERROR first\nLINE 2\n")
	a := writeText(t, dir, "a.log", tracebackLog)
	out, err := runTraceCmd(t, "--signatures=error-line,python-traceback", "--json", b, a)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	findings := doc["findings"].([]any)
	first := findings[0].(map[string]any)
	types := map[string]string{
		"signature": "string", "file": "string", "start_line": "float64", "end_line": "float64",
		"start_offset": "float64", "end_offset": "float64", "excerpt": "string", "lines": "[]interface {}",
	}
	for field, typ := range types {
		if got := fmt.Sprintf("%T", first[field]); got != typ {
			t.Errorf("finding field %s is %s, want %s (%v)", field, got, typ, first)
		}
	}
	if v, ok := first["time_ms"]; !ok || v != nil {
		t.Errorf("time_ms %v (present %v), want null", v, ok)
	}
	sig := doc["signatures"].(map[string]any)["python-traceback"].(map[string]any)
	if sig["name"] != "Python traceback" || sig["group"] != "traceback" || sig["kind"] != "block" || sig["level"] == nil {
		t.Errorf("signatures entry %v", sig)
	}
	summary := doc["finding_summary"].(map[string]any)["f2"].(map[string]any)
	counts := summary["signatures"].(map[string]any)["error-line"].(map[string]any)
	if summary["state"] != "partial" || counts["count"] != 2.0 || counts["stored"] != 2.0 {
		t.Errorf("finding_summary of f2 %v", summary)
	}
	var resp rxtypes.TraceResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	want := []string{"f1:error-line:1-1", "f2:error-line:2-2", "f2:python-traceback:3-5", "f2:error-line:7-7"}
	if got := findingKeys(resp); !slices.Equal(got, want) {
		t.Errorf("findings %v, want %v", got, want)
	}
}

// A trace without --signatures answers exactly the keys it answered
// before signatures existed.
func TestTrace_WithoutSignaturesKeepsTheOldKeys(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	out, err := runTraceCmd(t, "ERROR", path, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	old := []string{"after_context", "before_context", "cli_command", "context_lines", "file_chunks", "files",
		"matches", "max_results", "path", "patterns", "request_id", "scanned_files", "skip_reasons", "skipped_files", "time"}
	if got := slices.Sorted(maps.Keys(doc)); !slices.Equal(got, old) {
		t.Errorf("keys %v\nwant %v", got, old)
	}
	if string(doc["cli_command"]) != "null" {
		t.Errorf("cli_command %s, want null as before", doc["cli_command"])
	}
}

// The text budget keeps the lines of the first findings and marks the
// rest lines_truncated; each line is cut like line_text.
func TestTraceSignatures_TextBudgetAndLineCut(t *testing.T) {
	withTraceSignatures(t)
	var b strings.Builder
	for n := 1; n <= 2000; n++ {
		fmt.Fprintf(&b, "LINE %d ERROR %s\n", n, strings.Repeat("x", 1000))
	}
	path := writeText(t, t.TempDir(), "app.log", b.String())
	t.Setenv("RX_FINDINGS_TEXT_MAX_BYTES", "1048576")
	resp := traceSignaturesJSON(t, "--signatures=error-line", "--no-cache", path)
	kept, truncated := int64(0), 0
	for i, f := range resp.Findings {
		if f.LinesTruncated {
			truncated++
			if f.Lines != nil {
				t.Errorf("finding %d is truncated and has lines", i)
			}
			continue
		}
		if truncated > 0 {
			t.Fatalf("finding %d has lines after a truncated one", i)
		}
		kept += int64(len(f.Lines[0]))
	}
	if truncated == 0 || kept > 1<<20 || len(resp.Findings) != 2000 {
		t.Errorf("%d findings, %d truncated, %d bytes kept", len(resp.Findings), truncated, kept)
	}

	t.Setenv("RX_FINDINGS_TEXT_MAX_BYTES", "")
	t.Setenv("RX_MAX_LINE_TEXT_BYTES", "100")
	resp = traceSignaturesJSON(t, "--signatures=error-line", "--no-cache", path)
	for _, f := range resp.Findings {
		if f.LinesTruncated || len(f.Lines[0]) != 100 {
			t.Fatalf("line of %d bytes (truncated %v), want 100", len(f.Lines[0]), f.LinesTruncated)
		}
	}
}

// secretLog holds an invented AWS key on an error line and an invented
// private key.
const (
	secretKey  = "AKIAIOSFODNN7EXAMPLE"
	secretBody = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
	secretLog  = "LINE 1 ERROR login with key=" + secretKey + " refused\n" +
		"-----BEGIN PRIVATE KEY-----\n" + secretBody + "\n" + secretBody + "QQ\n" +
		"-----END PRIVATE KEY-----\n"
)

// A secret is masked in the excerpt, the lines and the human output; a
// private key's body lines become [masked, N bytes]; no secret byte is
// in either output.
func TestTraceSignatures_SecretsAreMaskedEverywhere(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", secretLog)
	args := []string{"--signatures=error-line,pem-key,aws-access-key", path}
	jsonOut, err := runTraceCmd(t, append(args, "--json")...)
	if err != nil {
		t.Fatal(err)
	}
	human, err := runTraceCmd(t, append(args, "--no-color")...)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"json": jsonOut, "human": human} {
		for _, secret := range []string{secretKey[4:], secretBody[:16]} {
			if strings.Contains(out, secret) {
				t.Errorf("%s output holds %q:\n%s", name, secret, out)
			}
		}
	}
	var resp rxtypes.TraceResponse
	if err := json.Unmarshal([]byte(jsonOut), &resp); err != nil {
		t.Fatal(err)
	}
	for _, f := range resp.Findings {
		switch f.Signature {
		case "error-line":
			if !f.Masked || !strings.Contains(f.Excerpt, "key=AKIA****") || !strings.Contains(f.Lines[0], "key=AKIA****") {
				t.Errorf("error-line masked %v excerpt %q lines %q", f.Masked, f.Excerpt, f.Lines)
			}
		case "pem-key":
			if want := []string{"[masked, 32 bytes]", "[masked, 34 bytes]", "[masked, 25 bytes]"}; !slices.Equal(f.Lines[1:], want) {
				t.Errorf("pem-key lines %q", f.Lines)
			}
		}
	}
	if !strings.Contains(human, "  3: [masked, 32 bytes]") || !strings.Contains(human, "key=AKIA****") {
		t.Errorf("human output:\n%s", human)
	}
}

// The lines around findings next to a private key (--before=4,
// --after=3) show its body lines as [masked, N bytes], in the JSON and
// in the human output, while the pem-key signature is only loaded, not
// selected: no byte of the key is printed.
func TestTraceSignatures_KeyBodyIsMaskedAroundOtherFindings(t *testing.T) {
	withTraceSignatures(t)
	keyA, keyB := strings.Repeat("RkFLRUtFWUFB", 6)[:64], strings.Repeat("RkFLRUtFWUJC", 6)[:64]
	text := "LINE 1 ERROR before\n-----BEGIN PRIVATE KEY-----\n" + keyA + "\n" + keyB +
		"\n-----END PRIVATE KEY-----\nLINE 6 ERROR after\n"
	path := writeText(t, t.TempDir(), "app.log", text)
	args := []string{"--signatures=error-line", "--before=4", "--after=3", path}
	jsonOut, err := runTraceCmd(t, append(args, "--json")...)
	if err != nil {
		t.Fatal(err)
	}
	human, err := runTraceCmd(t, append(args, "--no-color")...)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"json": jsonOut, "human": human} {
		for _, secret := range []string{keyA, keyB, keyA[:16], keyB[:16]} {
			if strings.Contains(out, secret) {
				t.Errorf("%s output holds %q:\n%s", name, secret, out)
			}
		}
	}
	var resp rxtypes.TraceResponse
	if err := json.Unmarshal([]byte(jsonOut), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Findings) != 2 {
		t.Fatalf("findings %+v", resp.Findings)
	}
	first, last := resp.Findings[0], resp.Findings[1]
	if want := []string{"----************", "[masked, 64 bytes]", "[masked, 64 bytes]"}; !slices.Equal(first.After, want) || !first.Masked {
		t.Errorf("line 1: after %q masked %v, want %q", first.After, first.Masked, want)
	}
	if want := []string{"----************", "[masked, 64 bytes]", "[masked, 64 bytes]", "[masked, 25 bytes]"}; !slices.Equal(last.Before, want) || !last.Masked {
		t.Errorf("line 6: before %q masked %v, want %q", last.Before, last.Masked, want)
	}
	for _, line := range []string{"  3- [masked, 64 bytes]", "  4- [masked, 64 bytes]", "  5- [masked, 25 bytes]"} {
		if !strings.Contains(human, line) {
			t.Errorf("human output lacks %q:\n%s", line, human)
		}
	}
}

// The human output of a block of lines 4-403: lines 4 to 23, then the
// count of the rest and the samples command for them; -B 1 adds line 3
// before it.
func TestTraceSignatures_HumanOutputOfALongBlock(t *testing.T) {
	withTraceSignatures(t)
	var b strings.Builder
	b.WriteString("LINE 1\nLINE 2\nLINE 3\nBEGIN block\n")
	for n := 5; n <= 402; n++ {
		fmt.Fprintf(&b, "LINE %d inside\n", n)
	}
	b.WriteString("END block\nLINE 404\n")
	path := writeText(t, t.TempDir(), "app.log", b.String())
	out, err := runTraceCmd(t, "--signatures=big-block", "--no-color", "-B", "1", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Findings: 1 in 1 file (big-block 1)\n",
		path + ":4-403  big-block  structure · low\n",
		"    3- LINE 3\n",
		"    4: BEGIN block\n",
		"   23: LINE 23 inside\n",
		"  … 380 more lines: rx samples " + path + " --lines=24-403\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LINE 24 inside") {
		t.Errorf("output prints line 24:\n%s", out)
	}
}

// --no-cache writes no findings cache; --max-results caps the matches
// and not the findings.
func TestTraceSignatures_NoCacheAndMaxResults(t *testing.T) {
	cache := withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	findingsDir := filepath.Join(cache, "rx", "findings")
	resp := traceSignaturesJSON(t, "--signatures=error-line", "--no-cache", "--max-results=1", "-e", "ERROR", path)
	if len(resp.Matches) != 1 || len(resp.Findings) != 2 {
		t.Errorf("matches %d findings %d, want 1 and 2", len(resp.Matches), len(resp.Findings))
	}
	if entries, _ := os.ReadDir(findingsDir); len(entries) != 0 {
		t.Errorf("--no-cache wrote %v", entries)
	}
	traceSignaturesJSON(t, "--signatures=error-line", path)
	if entries, _ := os.ReadDir(findingsDir); len(entries) != 1 {
		t.Errorf("a trace with the cache wrote %v, want one directory", entries)
	}
	if config.GetFindingsCacheDir() != findingsDir {
		t.Fatalf("findings cache at %s, the test looked at %s", config.GetFindingsCacheDir(), findingsDir)
	}
}

// cli_command spells the selection out (a bare flag is default) and the
// pattern goes with --regexp; a trace without --signatures keeps null.
func TestTraceSignatures_CLICommandSpellsOutTheSelection(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	resp := traceSignaturesJSON(t, "--signatures", "-e", "timeout", "-B", "2", path)
	want := "rx trace " + path + " --regexp=timeout --before=2 --signatures=default"
	if resp.CLICommand == nil || *resp.CLICommand != want {
		t.Errorf("cli_command %v, want %q", resp.CLICommand, want)
	}
}
