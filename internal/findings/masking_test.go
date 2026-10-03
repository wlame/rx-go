package findings

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fakeAPIKey is the invented value of the api-key signature (defKey) in
// secretLog.
const fakeAPIKey = "ABCDEFGHIJ012345"

// secretLog is keyLog, whose PEM block has its body on lines 4 to 7,
// with an error line holding an invented API key after it (line 10).
func secretLog() string {
	return keyLog() + "2026-10-10 14:00:04.000 ERROR token KEY-" + fakeAPIKey + " refused\n"
}

// secretIDs are the signatures the tests below scan secretLog for: an
// error line, a base64 run (on the key lines), the redacting block and
// the redacting regex.
var secretIDs = []string{"err-line", "b64-run", "pem-key", "api-key"}

// secretCatalogue loads the signatures of secretIDs.
func secretCatalogue(t *testing.T) *signatures.Catalogue {
	t.Helper()
	return loadCatalogue(t, defErrLine, defB64, defPem, defKey)
}

// maskingOptions are testOptions with masking set to mask.
func maskingOptions(t *testing.T, mask bool) ScanOptions {
	t.Helper()
	opts := testOptions(t)
	opts.MaskSecrets = mask
	return opts
}

// requireMode fails unless ans is a full answer of secretIDs over
// secretLog in its mode: masked, the key lines are the masked ranges,
// the base64 runs on them show their length and the API key shows its
// first characters; shown, nothing is masked and every excerpt is its
// line as it is.
func requireMode(t *testing.T, ans *Answer, masked bool) {
	t.Helper()
	if len(ans.Missing) != 0 {
		t.Fatalf("missing %v", ans.Missing)
	}
	b64 := ans.BySignature["b64-run"].Findings
	key := ans.BySignature["api-key"].Findings
	if len(b64) != 2 || len(key) != 1 {
		t.Fatalf("base64-run findings %+v, api-key findings %+v", b64, key)
	}
	if masked {
		if !slices.Equal(ans.MaskedRanges, keyLogRanges) || b64[0].Excerpt != "[masked, 64 bytes]" || !b64[0].Masked ||
			strings.Contains(key[0].Excerpt, fakeAPIKey) || !key[0].Masked {
			t.Errorf("masked answer: ranges %v, base64-run %+v, api-key %+v", ans.MaskedRanges, b64[0], key[0])
		}
		return
	}
	if len(ans.MaskedRanges) != 0 || b64[0].Excerpt != fakeKeyLine(1) || b64[1].Excerpt != fakeKeyLine(2) ||
		!strings.Contains(key[0].Excerpt, "KEY-"+fakeAPIKey) {
		t.Errorf("shown answer: ranges %v, base64-run %+v, api-key %+v", ans.MaskedRanges, b64, key[0])
	}
	for id, res := range ans.BySignature {
		for _, f := range res.Findings {
			if f.Masked {
				t.Errorf("shown answer: %s at line %d is marked masked (excerpt %q)", id, f.StartLine, f.Excerpt)
			}
		}
	}
}

// dirHolds reports whether a file under dir holds s.
func dirHolds(t *testing.T, dir, s string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = found || strings.Contains(string(data), s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// With masking off (the default of ScanOptions) no redacting signature
// masks: the excerpts are the lines as they are, no finding is marked
// masked, there is no masked range, and the cache keeps the text as the
// file holds it, with an empty ranges file.
func TestScan_WithMaskingOffTheAnswerAndTheCacheHoldTheText(t *testing.T) {
	cat := secretCatalogue(t)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", secretLog()))
	opts := maskingOptions(t, false)
	ans := mustScan(t, newScheduler(1), file, cat, pick(t, cat, secretIDs...), opts)
	requireMode(t, ans, false)

	var ranges rangesFile
	readJSONFile(t, filepath.Join(cacheDir(opts.Dir, file.Path()), rangesName), &ranges)
	if len(ranges.Ranges) != 0 || ranges.Key == "" {
		t.Errorf("ranges file %+v, want a key and no range", ranges)
	}
	for _, s := range []string{fakeKeyLine(1), fakeAPIKey} {
		if !dirHolds(t, opts.Dir, s) {
			t.Errorf("no cache file holds %q", s)
		}
	}
}

// Masked and shown answers never share a cache entry, in either order
// over one cache directory, with the read cache on: after a scan in one
// mode, a read in the other finds every signature missing, a scan in
// the other mode reads the file again and answers in its own mode, and
// the read cache, which still holds the first mode's entries, gives
// none of them to a read in the second.
func TestScan_MaskedAndShownAnswersNeverShareACacheEntry(t *testing.T) {
	cat := secretCatalogue(t)
	sel := pick(t, cat, secretIDs...)
	for _, order := range [][2]bool{{false, true}, {true, false}} {
		first, second := order[0], order[1]
		t.Run(map[bool]string{false: "shown then masked", true: "masked then shown"}[first], func(t *testing.T) {
			file := classify(t, writeLog(t, t.TempDir(), "app.log", secretLog()))
			s, rec := newRecordedScheduler(1)
			s.setReadCache(8, 64<<20)
			dir := t.TempDir()
			optsOf := func(mask bool) ScanOptions {
				opts := maskingOptions(t, mask)
				opts.Dir = dir
				return opts
			}
			read := func(mask bool) *Answer {
				t.Helper()
				ans, err := s.read(file, cat, sel, optsOf(mask))
				if err != nil {
					t.Fatal(err)
				}
				return ans
			}
			requireMissing := func(ans *Answer) {
				t.Helper()
				if !slices.Equal(ans.Missing, secretIDs) || len(ans.Checked) != 0 || ans.State != StateNone {
					t.Errorf("read in the other mode: checked %v missing %v state %s, want every signature missing",
						ans.Checked, ans.Missing, ans.State)
				}
			}
			scanIn := func(mask bool) {
				t.Helper()
				rec.scanned()
				requireMode(t, mustScan(t, s, file, cat, sel, optsOf(mask)), mask)
				if scans := rec.scanned(); len(scans) != 1 {
					t.Errorf("scan with masking %v: scans %v, want one", mask, scans)
				}
			}

			scanIn(first)
			requireMode(t, read(first), first) // decodes the entries into the read cache
			requireMissing(read(second))
			scanIn(second)
			requireMode(t, read(second), second)
			requireMissing(read(first))
			scanIn(first)
			requireMode(t, read(first), first)
		})
	}
}

// The text of a finding follows the mode too. With masking off, the
// lines of every finding and the lines around it are the file's lines
// as they are, the key's body included, and nothing is marked masked;
// with masking on, they are today's masked lines.
func TestTrace_TheTextOfAFindingFollowsTheMode(t *testing.T) {
	cat := secretCatalogue(t)
	path := writeLog(t, t.TempDir(), "app.log", secretLog())
	byKey := func(ans *TraceAnswer) map[string]rxtypes.Finding {
		out := map[string]rxtypes.Finding{}
		for _, f := range ans.Findings {
			out[fmt.Sprintf("%s:%d", f.Signature, f.StartLine)] = f
		}
		return out
	}
	lines := strings.Split(strings.TrimSuffix(secretLog(), "\n"), "\n")
	for _, mask := range []bool{false, true} {
		opts := traceOptions(t)
		opts.Scan.MaskSecrets = mask
		opts.Before, opts.After = 1, 1
		got := byKey(mustTrace(t, traceFiles(t, path), cat, pick(t, cat, "err-line", "pem-key", "api-key"), opts))
		pem, after, key := got["pem-key:3"], got["err-line:8"], got["err-line:10"]
		if !mask {
			if !slices.Equal(pem.Lines, lines[2:7]) || !slices.Equal(after.Before, lines[6:7]) || key.Lines[0] != lines[9] ||
				pem.Masked || after.Masked || key.Masked {
				t.Errorf("shown: pem-key %q (masked %v), err-line:8 before %q (masked %v), err-line:10 %q (masked %v)",
					pem.Lines, pem.Masked, after.Before, after.Masked, key.Lines, key.Masked)
			}
			continue
		}
		wantPem := []string{"----************", "[masked, 64 bytes]", "[masked, 64 bytes]", "[masked, 8 bytes]", "[masked, 25 bytes]"}
		if !slices.Equal(pem.Lines, wantPem) || !slices.Equal(after.Before, []string{"[masked, 25 bytes]"}) ||
			strings.Contains(key.Lines[0], fakeAPIKey) || !pem.Masked || !after.Masked || !key.Masked {
			t.Errorf("masked: pem-key %q (masked %v), err-line:8 before %q (masked %v), err-line:10 %q (masked %v)",
				pem.Lines, pem.Masked, after.Before, after.Masked, key.Lines, key.Masked)
		}
	}
}

// The mode is in every key: a signature's entry written with masking
// off is not current with masking on, and the reverse, since the
// redacting set the key hashes is empty with masking off.
func TestKey_MaskingIsInTheKey(t *testing.T) {
	cat := secretCatalogue(t)
	for _, id := range secretIDs {
		sig := pick(t, cat, id)[0]
		if Key(sig, cat, maskingOptions(t, false)) == Key(sig, cat, maskingOptions(t, true)) {
			t.Errorf("%s: one key with masking on and off", id)
		}
	}
}
