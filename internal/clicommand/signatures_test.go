package clicommand

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/signatures"
)

// signaturesFixture is the search path of the tests below, relative to
// the package directory go test runs in, so every path in the output is
// the same on every machine.
const signaturesFixture = "testdata/signatures"

// runSignatures runs `rx signatures ARGS` in process with the fixture as
// the search path and returns its standard output and error.
func runSignatures(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("RX_SIGNATURES_PATH", signaturesFixture)
	var out strings.Builder
	cmd := NewSignaturesCommand(&out, nil)
	// The root command of the binary silences cobra's own printing;
	// this command runs as the root here.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// exitCode is the exit code an error from a command produces.
func exitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return ExitGenericError
}

// The list shows each group in order with its count and opt-in mark,
// a row per signature with its level, kind, source and marks, then the
// errors and the search path. Rows are compared with their columns'
// spacing folded to one space, and every row starts its columns at the
// same places.
func TestSignaturesList_Human(t *testing.T) {
	out, err := runSignatures(t)
	if err != nil {
		t.Fatalf("rx signatures: %v", err)
	}
	want := []string{
		"traceback 8 signatures",
		"  python-traceback high block built-in",
		"secret 15 signatures",
		"  aws-access-key high regex built-in masked, disabled by testdata/signatures/team.yml:1",
		"pii 5 signatures, opt-in",
		"  email-address low regex built-in opt-in",
		"structure 9 signatures",
		"  blank-run low run testdata/signatures/team.yml:6 overrides",
		"time 3 signatures",
		"billing 3 signatures",
		"  invoice-failed high regex testdata/signatures/team.yml:12",
		"  slow-invoice medium regex testdata/signatures/team.yml:19 opt-in",
		"  any-digit medium regex testdata/signatures/team.yml:26 opt-in",
		"",
		"Errors (1)",
		"  testdata/signatures/broken.yml:6: broken-one: regex: error parsing regexp: missing closing ): `(oops`",
		"",
		"Search path: testdata/signatures (2 files)",
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	next := 0
	columns := -1
	for _, line := range lines {
		folded := strings.Join(strings.Fields(line), " ")
		if strings.HasPrefix(line, "  ") {
			folded = "  " + folded
		}
		if next < len(want) && folded == want[next] {
			next++
		}
		if fields := strings.Fields(line); strings.HasPrefix(line, "  ") && len(fields) >= 4 && !strings.HasPrefix(line, "  testdata/") {
			at := strings.Index(line, " "+fields[1]+" ") + 1
			if columns < 0 {
				columns = at
			} else if at != columns {
				t.Errorf("the level column starts at %d, not %d: %q", at, columns, line)
			}
		}
	}
	if next < len(want) {
		t.Errorf("output lacks %q (in this order after the lines before it):\n%s", want[next], out)
	}
}

// The list ends with the masking mode: the signatures marked masked
// mask what they find only when masking is on, and RX_MASK_SECRETS says
// whether it is, for `rx trace` and `rx serve` run without the flag.
func TestSignaturesList_SaysWhetherMaskedSignaturesMask(t *testing.T) {
	for env, want := range map[string]string{
		"":  "Secrets: shown; --mask-secrets or RX_MASK_SECRETS=1 masks what the signatures marked masked find",
		"1": "Secrets: masked by the signatures marked masked (RX_MASK_SECRETS)",
	} {
		t.Setenv("RX_MASK_SECRETS", env)
		out, err := runSignatures(t)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(out, "\n"+want+"\n") {
			t.Errorf("RX_MASK_SECRETS=%q: the list does not end with %q:\n%s", env, want, out)
		}
	}
}

// --group and --tag narrow the list; an unknown one is a usage error
// that lists close names.
func TestSignaturesList_Filters(t *testing.T) {
	out, err := runSignatures(t, "--group=billing", "--tag=perf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "billing  3 signatures\n  slow-invoice") || strings.Contains(out, "invoice-failed") || strings.Contains(out, "traceback") {
		t.Errorf("filtered list:\n%s", out)
	}
	for _, args := range [][]string{{"--group=bilEng"}, {"--tag=invoice"}} {
		if _, err := runSignatures(t, args...); exitCode(err) != ExitUsageError {
			t.Errorf("%v: exit %d (%v)", args, exitCode(err), err)
		}
	}
}

// --json gives the catalogue as one document: signatures with their
// fields, kind, source, keywords, line pass and marks; groups; levels;
// errors; the search path and the files read.
func TestSignaturesList_JSON(t *testing.T) {
	out, err := runSignatures(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Signatures []map[string]any `json:"signatures"`
		Groups     []map[string]any `json:"groups"`
		Levels     []string         `json:"levels"`
		Errors     []map[string]any `json:"errors"`
		SearchPath []string         `json:"search_path"`
		Files      []string         `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byID := map[string]map[string]any{}
	for _, s := range doc.Signatures {
		byID[s["id"].(string)] = s
	}
	invoice := byID["invoice-failed"]
	if invoice["kind"] != "regex" || invoice["line_pass"] != false || invoice["overrides"] != false || invoice["disabled_by"] != nil {
		t.Errorf("invoice-failed %v", invoice)
	}
	if got, _ := json.Marshal(invoice["keywords"]); string(got) != `[["invoice "],[" failed"]]` {
		t.Errorf("keywords %s", got)
	}
	if got, _ := json.Marshal(invoice["source"]); string(got) != `{"file":"testdata/signatures/team.yml","line":12}` {
		t.Errorf("source %s", got)
	}
	if got, _ := json.Marshal(byID["python-traceback"]["source"]); string(got) != `{"builtin":true}` {
		t.Errorf("built-in source %s", got)
	}
	if got, _ := json.Marshal(byID["aws-access-key"]["disabled_by"]); string(got) != `{"file":"testdata/signatures/team.yml","line":1}` {
		t.Errorf("disabled_by %s", got)
	}
	if byID["blank-run"]["overrides"] != true || byID["any-digit"]["line_pass"] != true {
		t.Errorf("marks: %v %v", byID["blank-run"], byID["any-digit"])
	}
	if len(doc.Groups) != 12 || doc.Groups[11]["name"] != "billing" || doc.Groups[11]["count"] != 3.0 || doc.Groups[11]["builtin"] != false {
		t.Errorf("groups %v", doc.Groups)
	}
	if !slices.Equal(doc.Levels, []string{"info", "low", "medium", "high", "critical"}) {
		t.Errorf("levels %v", doc.Levels)
	}
	if len(doc.Errors) != 1 || doc.Errors[0]["id"] != "broken-one" || doc.Errors[0]["line"] != 6.0 {
		t.Errorf("errors %v", doc.Errors)
	}
	if !slices.Equal(doc.SearchPath, []string{signaturesFixture}) || len(doc.Files) != 2 {
		t.Errorf("search path %v files %v", doc.SearchPath, doc.Files)
	}
}

// show prints a built-in's definition after defaults, its source, its
// keyword clauses and whether it runs in the line pass.
func TestSignaturesShow_Builtin(t *testing.T) {
	out, err := runSignatures(t, "show", "python-traceback")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# source: built-in\n",
		"# keywords (derived): [traceback (most recent call last):, exception group traceback (most recent call last):]\n",
		"# line pass: no\n",
		"# examples: not checked in this build\n",
		"id: python-traceback\n",
		"  max_lines: 400\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

// show of a user signature names its file and line and its marks; a
// keyword-less one says it runs in the line pass.
func TestSignaturesShow_User(t *testing.T) {
	out, err := runSignatures(t, "show", "blank-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# source: testdata/signatures/team.yml:6\n", "# overrides the built-in signature blank-run\n", "# keywords: none\n", "# line pass: yes\n", "run:\n  blank: true\n  min: 5\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	clauses, err := runSignatures(t, "show", "invoice-failed")
	if err != nil || !strings.Contains(clauses, "# keywords (derived): [invoice ] AND [ failed]\n") {
		t.Errorf("show invoice-failed: %v\n%s", err, clauses)
	}
	disabled, err := runSignatures(t, "show", "aws-access-key")
	if err != nil || !strings.Contains(disabled, "# disabled by testdata/signatures/team.yml:1\n") {
		t.Errorf("show aws-access-key: %v\n%s", err, disabled)
	}
	asJSON, err := runSignatures(t, "show", "invoice-failed", "--json")
	var doc map[string]any
	if err != nil || json.Unmarshal([]byte(asJSON), &doc) != nil || doc["id"] != "invoice-failed" || doc["kind"] != "regex" {
		t.Errorf("show --json: %v\n%s", err, asJSON)
	}
}

// show prints the cache key the signature's findings are stored under
// with the settings of the environment; a setting that changes answers
// changes it.
func TestSignaturesShow_CacheKey(t *testing.T) {
	keyLine := regexp.MustCompile(`(?m)^# cache key: ([0-9a-f]{64})$`)
	out, err := runSignatures(t, "show", "invoice-failed")
	m := keyLine.FindStringSubmatch(out)
	if err != nil || m == nil {
		t.Fatalf("show has no cache key line (%v):\n%s", err, out)
	}
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{signaturesFixture}})
	sig, _ := cat.Lookup("invoice-failed")
	if want := findings.Key(sig, cat, findingsScanOptions()); m[1] != want {
		t.Errorf("printed key %s, want %s", m[1], want)
	}
	if !strings.Contains(out, "# line pass: no\n# cache key: ") {
		t.Errorf("the cache key does not follow the line pass:\n%s", out)
	}
	t.Setenv("RX_FINDINGS_MAX_PER_SIGNATURE", "500")
	capped, err := runSignatures(t, "show", "invoice-failed")
	if c := keyLine.FindStringSubmatch(capped); err != nil || c == nil || c[1] == m[1] {
		t.Errorf("another cap kept the key (%v):\n%s", err, capped)
	}
}

// show of an unknown id and of a refused one are usage errors.
func TestSignaturesShow_UnknownAndRefused(t *testing.T) {
	for _, id := range []string{"invoice-faild", "broken-one"} {
		if _, err := runSignatures(t, "show", id); exitCode(err) != ExitUsageError {
			t.Errorf("show %s: exit %d (%v)", id, exitCode(err), err)
		}
	}
}

// check prints errors and warnings and exits 1 on an error, 0 without;
// a file named that does not exist is an error.
func TestSignaturesCheck(t *testing.T) {
	out, err := runSignatures(t, "check")
	if exitCode(err) != ExitGenericError {
		t.Errorf("check of the fixture: exit %d (%v)", exitCode(err), err)
	}
	for _, want := range []string{
		"testdata/signatures/broken.yml:6: broken-one: error: regex: error parsing regexp",
		"testdata/signatures/team.yml:26: any-digit: warning: no keywords: runs in the line pass\n",
		"examples: not checked in this build\n",
		"2 files, 4 signatures: 1 error, 1 warning\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check lacks %q:\n%s", want, out)
		}
	}

	clean, err := runSignatures(t, "check", filepath.Join(signaturesFixture, "team.yml"))
	if exitCode(err) != ExitSuccess || !strings.Contains(clean, "1 file, 4 signatures: 0 errors, 1 warning\n") {
		t.Errorf("check team.yml: exit %d (%v)\n%s", exitCode(err), err, clean)
	}

	missing, err := runSignatures(t, "check", filepath.Join(t.TempDir(), "nothing.yml"))
	if exitCode(err) != ExitGenericError || !strings.Contains(missing, "nothing.yml: error: no such file") {
		t.Errorf("check of a missing file: exit %d (%v)\n%s", exitCode(err), err, missing)
	}
}

// The template's output loads cleanly through check.
func TestSignaturesTemplate_LoadsThroughCheck(t *testing.T) {
	out, err := runSignatures(t, "template")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(signatures.Template()) {
		t.Error("template prints something other than the template")
	}
	path := filepath.Join(t.TempDir(), "mine.yml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	checked, err := runSignatures(t, "check", path)
	if exitCode(err) != ExitSuccess || !strings.Contains(checked, "0 errors, 0 warnings") {
		t.Errorf("check of the template: exit %d (%v)\n%s", exitCode(err), err, checked)
	}
}

// The examples check runs when the command is given a checker, and the
// output says so.
func TestSignaturesCheck_WithExamplesChecker(t *testing.T) {
	t.Setenv("RX_SIGNATURES_PATH", signaturesFixture)
	var out strings.Builder
	calls := 0
	cmd := NewSignaturesCommand(&out, func(*signatures.Signature) error { calls++; return nil })
	cmd.SetArgs([]string{"check", filepath.Join(signaturesFixture, "team.yml")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls == 0 || !strings.Contains(out.String(), "examples: checked\n") {
		t.Errorf("calls %d\n%s", calls, out.String())
	}
}
