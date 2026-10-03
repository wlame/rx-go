package clicommand

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// secretSelection is the selection of the masking tests: an error line,
// the private-key block and the AWS key, over secretLog.
const secretSelection = "--signatures=error-line,pem-key,aws-access-key"

// requireTraceMode fails unless a trace answer over secretLog is in its
// mode: masked, the key shows its first characters and the private
// key's body lines their length; shown, both are as the file holds
// them and nothing is marked masked. cli_command carries --mask-secrets
// when masked, and only then.
func requireTraceMode(t *testing.T, resp rxtypes.TraceResponse, masked bool) {
	t.Helper()
	byID := map[string]rxtypes.Finding{}
	for _, f := range resp.Findings {
		byID[f.Signature] = f
	}
	errLine, pem := byID["error-line"], byID["pem-key"]
	if len(errLine.Lines) != 1 || len(pem.Lines) != 4 || resp.CLICommand == nil {
		t.Fatalf("findings %+v", resp.Findings)
	}
	cli := *resp.CLICommand
	if masked {
		if !errLine.Masked || !strings.Contains(errLine.Excerpt, "key=AKIA****") || strings.Contains(errLine.Lines[0], secretKey) ||
			!slices.Equal(pem.Lines[1:], []string{"[masked, 32 bytes]", "[masked, 34 bytes]", "[masked, 25 bytes]"}) ||
			!slices.Contains(strings.Fields(cli), "--mask-secrets") {
			t.Errorf("masked: error-line %+v, pem-key lines %q, cli_command %q", errLine, pem.Lines, cli)
		}
		return
	}
	if errLine.Masked || pem.Masked || !strings.Contains(errLine.Excerpt, "key="+secretKey) || !strings.Contains(errLine.Lines[0], "key="+secretKey) ||
		!slices.Equal(pem.Lines[1:], []string{secretBody, secretBody + "QQ", "-----END PRIVATE KEY-----"}) ||
		slices.Contains(strings.Fields(cli), "--mask-secrets") {
		t.Errorf("shown: error-line %+v, pem-key %+v, cli_command %q", errLine, pem, cli)
	}
}

// cacheFiles returns the contents of every file under dir, by path.
func cacheFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Without --mask-secrets and RX_MASK_SECRETS, a file with a fake key
// gives the key as the file holds it in the excerpt, the lines and the
// human output, and in the findings cache; the private key's body lines
// are shown, nothing is marked masked, and the ranges file of the cache
// holds no range.
func TestTraceSignatures_SecretsAreShownByDefault(t *testing.T) {
	cache := withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", secretLog)
	requireTraceMode(t, traceSignaturesJSON(t, secretSelection, path), false)

	human, err := runTraceCmd(t, secretSelection, "--no-color", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"key=" + secretKey, "  3: " + secretBody} {
		if !strings.Contains(human, want) {
			t.Errorf("human output lacks %q:\n%s", want, human)
		}
	}
	if strings.Contains(human, "[masked") || strings.Contains(human, "****") {
		t.Errorf("human output masks:\n%s", human)
	}

	holdsKey, ranges := false, 0
	for name, body := range cacheFiles(t, cache) {
		holdsKey = holdsKey || strings.Contains(body, secretKey)
		if filepath.Base(name) == "meta.ranges.json" {
			ranges++
			var file struct {
				Ranges [][2]int64 `json:"ranges"`
			}
			if err := json.Unmarshal([]byte(body), &file); err != nil || len(file.Ranges) != 0 {
				t.Errorf("%s: %s, want no range (%v)", name, body, err)
			}
		}
	}
	if !holdsKey || ranges != 1 {
		t.Errorf("findings cache: holds the key %v, %d ranges files; want the key and one file", holdsKey, ranges)
	}
}

// --mask-secrets and RX_MASK_SECRETS=1 each give the masked answer; the
// flag wins over the variable either way; a value of the variable that
// is not a boolean leaves masking off.
func TestTraceSignatures_MaskSecretsFlagAndVariable(t *testing.T) {
	cases := []struct {
		name   string
		flags  []string
		env    string
		masked bool
	}{
		{"neither", nil, "", false},
		{"the flag", []string{"--mask-secrets"}, "", true},
		{"RX_MASK_SECRETS=1", nil, "1", true},
		{"RX_MASK_SECRETS=true", nil, "true", true},
		{"RX_MASK_SECRETS=0", nil, "0", false},
		{"the flag over RX_MASK_SECRETS=0", []string{"--mask-secrets"}, "0", true},
		{"--mask-secrets=false over RX_MASK_SECRETS=1", []string{"--mask-secrets=false"}, "1", false},
		{"RX_MASK_SECRETS=maybe", nil, "maybe", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTraceSignatures(t)
			t.Setenv("RX_MASK_SECRETS", tc.env)
			path := writeText(t, t.TempDir(), "app.log", secretLog)
			args := append(append([]string{secretSelection}, tc.flags...), path)
			requireTraceMode(t, traceSignaturesJSON(t, args...), tc.masked)
		})
	}
}

// Over one cache directory, a masked run after a shown one and a shown
// run after a masked one each answer in their own mode: an answer is
// never taken from the other mode's cache entries.
func TestTraceSignatures_MaskedAndShownRunsShareNoCacheEntry(t *testing.T) {
	withTraceSignatures(t)
	path := writeText(t, t.TempDir(), "app.log", secretLog)
	for _, masked := range []bool{false, true, true, false, false, true} {
		args := []string{secretSelection, path}
		if masked {
			args = append(args, "--mask-secrets")
		}
		requireTraceMode(t, traceSignaturesJSON(t, args...), masked)
	}
}

// `rx serve` takes the mode from --mask-secrets, or from
// RX_MASK_SECRETS when the flag is not given, as `rx trace` does.
func TestServeCommand_MaskSecretsFlagWinsOverTheVariable(t *testing.T) {
	cases := []struct {
		args []string
		env  string
		want bool
	}{
		{nil, "", false},
		{nil, "1", true},
		{[]string{"--mask-secrets"}, "", true},
		{[]string{"--mask-secrets"}, "0", true},
		{[]string{"--mask-secrets=false"}, "yes", false},
	}
	for _, tc := range cases {
		t.Setenv("RX_MASK_SECRETS", tc.env)
		cmd := NewServeCommand(io.Discard, "test")
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		if got := maskSecretsMode(cmd); got != tc.want {
			t.Errorf("rx serve %v with RX_MASK_SECRETS=%q: masking %v, want %v", tc.args, tc.env, got, tc.want)
		}
	}
}

// The start banner of `rx serve` says the mode.
func TestServeBanner_SaysWhetherSecretsAreMasked(t *testing.T) {
	if got := secretsBannerLine(true); got != "Secrets: masked" {
		t.Errorf("masking on: %q", got)
	}
	if got, want := secretsBannerLine(false), "Secrets: shown; start with --mask-secrets or RX_MASK_SECRETS=1 to mask them"; got != want {
		t.Errorf("masking off: %q, want %q", got, want)
	}
}
