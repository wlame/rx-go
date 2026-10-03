package clicommand

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/cachefill"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
	"github.com/wlame/rx-go/internal/trace"
)

// runScanCmd runs `rx scan ARGS` in process and returns what it printed
// and its error.
func runScanCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewScanCommand(&buf)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// scanJSON runs `rx scan ARGS --json` and parses the answer; any error
// fails the test.
func scanJSON(t *testing.T, args ...string) scanResult {
	t.Helper()
	out, err := runScanCmd(t, append(args, "--json")...)
	if err != nil {
		t.Fatalf("rx scan %v: %v\n%s", args, err, out)
	}
	var result scanResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse --json: %v\n%s", err, out)
	}
	return result
}

// exitCodeOf is the exit code err carries, 0 for nil.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return ExitSuccess
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error %v carries no exit code", err)
	}
	return exitErr.Code
}

// cachedAnswer reads the findings cache for path and the selection as a
// later GET /v1/findings reads it, with the options the commands use
// and the masking mode given.
func cachedAnswer(t *testing.T, path, selection string, mask bool) *findings.Answer {
	t.Helper()
	cat := signatures.Load(signatures.LoadOptions{CheckExamples: sigscan.CheckExamples})
	sel, err := cat.Resolve(selection)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := paths.Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := trace.ClassifyForSearch(pin)
	if err != nil {
		t.Fatal(err)
	}
	opts := findingsScanOptions()
	opts.MaskSecrets = mask
	answer, err := findings.Read(file, cat, sel, opts)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// rx scan FILE scans the file and keeps its findings in the cache: a
// later read of the cache answers them, the answer gives the state and
// the counts, and a second run finds the file current. --force scans it
// again.
func TestScan_FileFillsTheCache(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)

	if before := cachedAnswer(t, path, "default", false); before.State != findings.StateNone {
		t.Fatalf("cache before the scan: %s", before.State)
	}
	first := scanJSON(t, path)
	if len(first.Files) != 1 {
		t.Fatalf("files %+v", first.Files)
	}
	got := first.Files[0]
	if got.Path != path || got.Outcome != "scanned" || got.State != "full" ||
		got.Counts["error-line"] != 2 || got.Counts["python-traceback"] != 1 || got.Findings != 3 {
		t.Errorf("first scan %+v", got)
	}
	if first.Signatures != "default" || first.Totals.FilesTotal != 1 || first.Totals.Done != 1 || first.Totals.Findings != 3 {
		t.Errorf("first scan totals %+v, selection %q", first.Totals, first.Signatures)
	}
	after := cachedAnswer(t, path, "default", false)
	if after.State != findings.StateFull || len(after.Missing) != 0 || after.BySignature["error-line"].Count != 2 {
		t.Errorf("cache after the scan: %+v", after)
	}

	again := scanJSON(t, path)
	if again.Files[0].Outcome != "current" || again.Totals.Skipped != 1 || again.Totals.Current != 1 || again.Totals.Done != 0 {
		t.Errorf("second scan %+v totals %+v", again.Files, again.Totals)
	}
	if again.Files[0].Findings != 3 || len(again.Files[0].Counts) != 2 {
		t.Errorf("a current file keeps its counts, those of the signatures that found something: %+v", again.Files[0])
	}
	if forced := scanJSON(t, "--force", path); forced.Files[0].Outcome != "scanned" || forced.Totals.Done != 1 {
		t.Errorf("forced scan %+v", forced.Files)
	}
}

// rx scan --recursive DIR scans every file the tree lists under DIR, in
// the walk's order, and skips a file that is not text with the reason;
// the same files in the same order as rx index --recursive lists. A
// hidden file is left out unless --hidden.
func TestScan_RecursiveScansEveryListedFileAndSkipsNonText(t *testing.T) {
	withTraceSignatures(t)
	dir := t.TempDir()
	app := writeText(t, dir, "app.log", tracebackLog)
	deep := writeText(t, dir, "sub/deeper/b.log", "LINE 1 ERROR deep\n")
	quiet := writeText(t, dir, "sub/q.log", "LINE 1 nothing\n")
	binary := writeText(t, dir, "sub/c.bin", "ERROR\x00binary\n")
	writeText(t, dir, ".hidden.log", "LINE 1 ERROR hidden\n")

	result := scanJSON(t, "--recursive", "--signatures=error-line", dir)
	var scanned []string
	for _, f := range result.Files {
		scanned = append(scanned, f.Path)
	}
	if want := []string{app, deep, quiet}; !slices.Equal(scanned, want) {
		t.Errorf("scanned %v, want %v", scanned, want)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Path != binary || !strings.Contains(result.Skipped[0].Reason, "not a text file") {
		t.Errorf("skipped %+v", result.Skipped)
	}
	want := scanTotals{Counts: cachefill.Counts{FilesTotal: 4, Done: 3, Skipped: 1}, Findings: 3}
	if result.Totals != want {
		t.Errorf("totals %+v, want %+v", result.Totals, want)
	}

	zero := 0
	indexed := runIndexJSON(t, indexParams{paths: []string{dir}, recursive: true, threshold: &zero})
	var indexOrder []string
	for _, entry := range indexed["indexed"].([]any) {
		indexOrder = append(indexOrder, entry.(map[string]any)["path"].(string))
	}
	if !slices.Equal(indexOrder, scanned) {
		t.Errorf("rx index --recursive lists %v, rx scan --recursive %v", indexOrder, scanned)
	}

	paths.SetIncludeHidden(true)
	t.Cleanup(func() { paths.SetIncludeHidden(false) })
	if withHidden := scanJSON(t, "--recursive", "--signatures=error-line", dir); len(withHidden.Files) != 4 {
		t.Errorf("--hidden scanned %d files, want 4", len(withHidden.Files))
	}
}

// A directory without --recursive scans the files directly inside it,
// as rx index DIR indexes them, and leaves its subdirectories alone; the
// listing is the one rx index DIR makes, in the same order.
func TestScan_ADirectoryWithoutRecursiveScansItsTopLevel(t *testing.T) {
	withTraceSignatures(t)
	dir := t.TempDir()
	app := writeText(t, dir, "app.log", tracebackLog)
	top := writeText(t, dir, "b.log", "LINE 1 ERROR top\n")
	deep := writeText(t, dir, "sub/c.log", "LINE 1 ERROR deep\n")

	result := scanJSON(t, dir)
	var scanned []string
	for _, f := range result.Files {
		scanned = append(scanned, f.Path)
	}
	if want := []string{app, top}; !slices.Equal(scanned, want) {
		t.Errorf("scanned %v, want the top level %v", scanned, want)
	}
	if got := cachedAnswer(t, deep, "default", false); got.State != findings.StateNone {
		t.Errorf("a file of a subdirectory was scanned: %s", got.State)
	}

	zero := 0
	indexed := runIndexJSON(t, indexParams{paths: []string{dir}, threshold: &zero})
	var indexOrder []string
	for _, entry := range indexed["indexed"].([]any) {
		indexOrder = append(indexOrder, entry.(map[string]any)["path"].(string))
	}
	if !slices.Equal(indexOrder, scanned) {
		t.Errorf("rx index DIR lists %v, rx scan DIR %v", indexOrder, scanned)
	}
}

// The exit codes are rx index's: a missing file exits 3 after the other
// files are scanned, a selection that names an unknown signature exits
// 2, a path outside the search roots exits 4.
func TestScan_ExitCodesAreRxIndexs(t *testing.T) {
	withTraceSignatures(t)
	dir := t.TempDir()
	app := writeText(t, dir, "app.log", tracebackLog)
	missing := filepath.Join(dir, "missing.log")

	out, err := runScanCmd(t, "--json", app, missing)
	if code := exitCodeOf(t, err); code != ExitFileNotFound {
		t.Errorf("a missing file: exit %d (%v)", code, err)
	}
	var result scanResult
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if len(result.Files) != 1 || len(result.Errors) != 1 || result.Errors[0].Path != missing || result.Totals.Failed != 1 {
		t.Errorf("a missing file beside a scanned one: %+v", result)
	}

	if _, err := runScanCmd(t, "--signatures=no-such-signature", app); exitCodeOf(t, err) != ExitUsageError {
		t.Errorf("an unknown signature: %v", err)
	}

	if err := paths.SetSearchRoots([]string{dir}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.Reset)
	outside := writeText(t, t.TempDir(), "x.log", "LINE 1 ERROR\n")
	if _, err := runScanCmd(t, outside); exitCodeOf(t, err) != ExitAccessDenied {
		t.Errorf("outside the roots: %v", err)
	}
}

// --mask-secrets reaches the scan: the findings it caches are those of a
// masking run, which a read without masking does not take for its own.
func TestScan_MaskSecretsReachesTheCache(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", tracebackLog)
	scanJSON(t, "--mask-secrets", path)
	if got := cachedAnswer(t, path, "default", true); got.State != findings.StateFull {
		t.Errorf("masked cache after a masked scan: %s", got.State)
	}
	if got := cachedAnswer(t, path, "default", false); got.State != findings.StateNone {
		t.Errorf("unmasked cache after a masked scan: %s", got.State)
	}
	t.Setenv("RX_MASK_SECRETS", "1")
	if again := scanJSON(t, path); again.Files[0].Outcome != "current" {
		t.Errorf("RX_MASK_SECRETS=1 after a masked scan: %+v", again.Files)
	}
}

// The human output is one line per file with its state and counts, a
// line per skipped file, and a total line.
func TestScan_HumanOutputOneLinePerFileAndATotal(t *testing.T) {
	withTraceSignatures(t)
	dir := t.TempDir()
	app := writeText(t, dir, "app.log", tracebackLog)
	binary := writeText(t, dir, "c.bin", "ERROR\x00binary\n")
	out, err := runScanCmd(t, "-r", dir)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	want := []string{
		"  " + app + ": full, 3 findings (error-line 2, python-traceback 1)",
		"Skipped 1 files:",
		"  " + binary + ": not a text file: a NUL byte in its first 8 KiB",
	}
	if len(lines) != len(want)+1 || !slices.Equal(lines[:len(want)], want) {
		t.Fatalf("output:\n%s", out)
	}
	if total := lines[len(want)]; !strings.HasPrefix(total, "Total: 2 files, 1 scanned, 1 skipped (0 current), 0 failed, 3 findings in ") {
		t.Errorf("total line %q", total)
	}
	again, err := runScanCmd(t, "-r", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(again, "  "+app+": full, 3 findings (error-line 2, python-traceback 1), current\n") {
		t.Errorf("a current file's line:\n%s", again)
	}
}

// The command's flags: --signatures takes its list after `=` and a bare
// --signatures is the default set; -f, -r and --json are there.
func TestScan_Flags(t *testing.T) {
	cmd := NewScanCommand(os.Stdout)
	for _, name := range []string{"signatures", "force", "recursive", "json", "mask-secrets"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("no --%s", name)
		}
	}
	if got := cmd.Flags().Lookup("signatures").NoOptDefVal; got != "default" {
		t.Errorf("bare --signatures reads %q", got)
	}
	if cmd.Flags().ShorthandLookup("f") == nil || cmd.Flags().ShorthandLookup("r") == nil {
		t.Error("no -f or -r")
	}
}
