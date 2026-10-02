package findings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// defB64 finds a run of 40 base64 bytes or more on a line. It does not
// redact, so on a key line its excerpt would show the key.
const defB64 = `
  - id: b64-run
    name: Base64 run
    group: structure
    description: A long base64 run.
    measure: {name: base64-run, min_length: 40}
`

// fakeKeyLine is the i-th invented key line of a PEM body: 64 base64
// characters, distinct for every i, which no real key holds.
func fakeKeyLine(i int) string {
	return strings.Repeat(fmt.Sprintf("RkFLRS%04dS0VZ", i), 5)[:64]
}

// keyLog is a log with a PEM block on lines 3 to 7, whose body is lines
// 4 to 7, and an error line before and after it.
func keyLog() string {
	return strings.Join([]string{
		"2026-10-10 14:00:00.000 INFO start",
		"2026-10-10 14:00:01.000 ERROR before the key",
		"-----BEGIN PRIVATE KEY-----",
		fakeKeyLine(1),
		fakeKeyLine(2),
		"QUJDRA==",
		"-----END PRIVATE KEY-----",
		"2026-10-10 14:00:02.000 ERROR after the key",
		"2026-10-10 14:00:03.000 INFO done",
	}, "\n") + "\n"
}

// keyLogRanges are the masked ranges of keyLog.
var keyLogRanges = [][2]int64{{4, 7}}

// keyLogSecrets are the body lines of keyLog.
var keyLogSecrets = []string{fakeKeyLine(1), fakeKeyLine(2), "QUJDRA=="}

// requireNoSecretInDir fails when a file under dir holds any of the
// secrets.
func requireNoSecretInDir(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			if strings.Contains(string(data), s) {
				t.Errorf("%s holds the secret %q", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A base64-run finding on a key line of a PEM block is cached with the
// excerpt "[masked, 64 bytes]", whether the redacting block is selected
// or only loaded; the answer carries the body's lines as its masked
// ranges, the ranges file holds them, and no byte of the body is in any
// file of the cache.
func TestScan_RedactingBlockBodyNeverReachesTheCache(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defPem, defB64)
	for name, ids := range map[string][]string{
		"block not selected": {"b64-run", "err-line"},
		"block selected":     {"b64-run", "err-line", "pem-key"},
	} {
		t.Run(name, func(t *testing.T) {
			file := classify(t, writeLog(t, t.TempDir(), "app.log", keyLog()))
			opts := testOptions(t)
			ans := mustScan(t, newScheduler(1), file, cat, pick(t, cat, ids...), opts)
			if !slices.Equal(ans.MaskedRanges, keyLogRanges) {
				t.Errorf("masked ranges %v, want %v", ans.MaskedRanges, keyLogRanges)
			}
			b64 := ans.BySignature["b64-run"].Findings
			if len(b64) != 2 || b64[0].Excerpt != "[masked, 64 bytes]" || b64[1].Excerpt != "[masked, 64 bytes]" || !b64[0].Masked {
				t.Errorf("base64-run findings %+v", b64)
			}
			var ranges rangesFile
			readJSONFile(t, filepath.Join(cacheDir(opts.Dir, file.Path()), rangesName), &ranges)
			if !slices.Equal(ranges.Ranges, keyLogRanges) || ranges.Key == "" {
				t.Errorf("ranges file %+v", ranges)
			}
			requireNoSecretInDir(t, opts.Dir, keyLogSecrets...)
		})
	}
}

// A cache hit gives the same masked excerpts and the same masked ranges
// as a cold scan, with no scan: also after a second scan of another
// signature rewrote meta.json and cleaned the directory. Read gives them
// too.
func TestScan_CacheHitGivesTheSameMaskedTextAndRanges(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defPem, defB64)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", keyLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	mustScan(t, s, file, cat, pick(t, cat, "b64-run"), opts)
	mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
	rec.scanned()

	warm := mustScan(t, s, file, cat, pick(t, cat, "b64-run", "err-line"), opts)
	if scans := rec.scanned(); len(scans) != 0 {
		t.Fatalf("a cache hit scanned %v", scans)
	}
	cold := coldAnswer(t, keyLog(), cat, "b64-run", "err-line")
	requireSameFindings(t, warm, cold)
	read, err := s.read(file, cat, pick(t, cat, "b64-run", "err-line"), opts)
	if err != nil || len(read.Missing) != 0 {
		t.Fatalf("read: %+v (%v)", read, err)
	}
	requireSameFindings(t, read, cold)
}

// The masked ranges are stale when a redacting signature changes: a
// redacting block whose body ends earlier, and a redacting regex added
// to the catalogue (another redacting set), make the next scan read
// the file again, and give the new ranges.
func TestScan_ChangedRedactingSignatureMakesTheRangesStale(t *testing.T) {
	file := classify(t, writeLog(t, t.TempDir(), "app.log", keyLog()))
	s, rec := newRecordedScheduler(1)
	opts := testOptions(t)
	cat := loadCatalogue(t, defErrLine, defPem, defB64)
	mustScan(t, s, file, cat, pick(t, cat, "err-line"), opts)
	rec.scanned()

	shorter := loadCatalogue(t, defErrLine, strings.Replace(defPem, "    block:\n", "    block:\n      max_lines: 2\n", 1), defB64)
	ans := mustScan(t, s, file, shorter, pick(t, shorter, "err-line"), opts)
	if scans := rec.scanned(); len(scans) != 1 {
		t.Errorf("a changed redacting block: scans %v, want one", scans)
	}
	if !slices.Equal(ans.MaskedRanges, [][2]int64{{4, 4}}) {
		t.Errorf("masked ranges %v, want [[4 4]]", ans.MaskedRanges)
	}

	more := loadCatalogue(t, defErrLine, defPem, defB64, defKey)
	ans = mustScan(t, s, file, more, pick(t, more, "err-line"), opts)
	if scans := rec.scanned(); len(scans) != 1 {
		t.Errorf("another redacting set: scans %v, want one", scans)
	}
	if !slices.Equal(ans.MaskedRanges, keyLogRanges) {
		t.Errorf("masked ranges %v, want %v", ans.MaskedRanges, keyLogRanges)
	}
}

// rewriteJSON decodes the JSON file at path into a generic value, lets
// change edit it, and writes it back. Numbers stay exact (json.Number):
// a float64 would round meta.json's nanosecond times, and the file's
// identity would no longer match.
func rewriteJSON(t *testing.T, path string, change func(v map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	change(v)
	data, err = json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A cached entry is used only with its file's masked ranges, since its
// text could not be masked without them. When the ranges file is
// missing, holds another count than meta.json records or ranges that
// are not sorted, or meta.json names no ranges (as one written before
// they were kept), Read answers every signature missing, and Scan scans
// again and writes the ranges back; a meta.json without ranges shows as
// nothing cached in Summary too.
func TestScan_EntriesWithoutTheirRangesAreMissing(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defPem, defB64)
	cases := map[string]func(t *testing.T, dir string){
		"ranges file missing": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, rangesName)); err != nil {
				t.Fatal(err)
			}
		},
		"another count": func(t *testing.T, dir string) {
			rewriteJSON(t, filepath.Join(dir, metaName), func(v map[string]any) {
				v["masked_ranges"].(map[string]any)["count"] = 2
			})
		},
		"ranges not sorted": func(t *testing.T, dir string) {
			rewriteJSON(t, filepath.Join(dir, rangesName), func(v map[string]any) {
				v["ranges"] = []any{[]any{9, 9}, []any{4, 7}}
			})
			rewriteJSON(t, filepath.Join(dir, metaName), func(v map[string]any) {
				v["masked_ranges"].(map[string]any)["count"] = 2
			})
		},
		"meta.json without ranges": func(t *testing.T, dir string) {
			rewriteJSON(t, filepath.Join(dir, metaName), func(v map[string]any) { delete(v, "masked_ranges") })
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			file := classify(t, writeLog(t, t.TempDir(), "app.log", keyLog()))
			s, rec := newRecordedScheduler(1)
			opts := testOptions(t)
			mustScan(t, s, file, cat, pick(t, cat, "b64-run"), opts)
			rec.scanned()
			spoil(t, cacheDir(opts.Dir, file.Path()))

			read, err := s.read(file, cat, pick(t, cat, "b64-run"), opts)
			if err != nil || !slices.Equal(read.Missing, []string{"b64-run"}) || read.State != StateNone {
				t.Fatalf("read: %+v (%v)", read, err)
			}
			// Summary reads meta.json alone, so only a meta.json without
			// ranges shows there; the other cases are found when the
			// ranges file is read.
			if sum, err := s.summary(file.Pinned(), cat, opts); err != nil || (name == "meta.json without ranges" && sum != nil) {
				t.Errorf("summary %+v (%v)", sum, err)
			}
			ans := mustScan(t, s, file, cat, pick(t, cat, "b64-run"), opts)
			if scans := rec.scanned(); len(scans) != 1 {
				t.Errorf("scans %v, want one", scans)
			}
			requireSameFindings(t, ans, coldAnswer(t, keyLog(), cat, "b64-run"))
			again, err := s.read(file, cat, pick(t, cat, "b64-run"), opts)
			if err != nil || len(again.Missing) != 0 {
				t.Errorf("read after the scan: %+v (%v)", again, err)
			}
		})
	}
}

// Masked ranges read from the cache must be as a scan writes them; a
// ranges file that holds anything else is not used.
func TestValidRanges(t *testing.T) {
	cases := []struct {
		ranges [][2]int64
		ok     bool
	}{
		{nil, true},
		{[][2]int64{{2, 2}, {4, 9}}, true},
		{[][2]int64{{0, 3}}, false},
		{[][2]int64{{5, 4}}, false},
		{[][2]int64{{4, 9}, {2, 2}}, false},
		{[][2]int64{{2, 4}, {5, 6}}, false},
		{[][2]int64{{2, 4}, {4, 6}}, false},
	}
	for _, tc := range cases {
		if got := validRanges(tc.ranges); got != tc.ok {
			t.Errorf("validRanges(%v) = %v, want %v", tc.ranges, got, tc.ok)
		}
	}
}

// Around findings next to a PEM block, every body line is shown as
// "[masked, N bytes]" in the lines before (-B 4) and after (-A 3) a
// finding and in the lines of a finding on a key line, whether the
// block is selected or not; such findings are marked masked, and no
// byte of the body is in the answer. Lines outside the body are shown
// as they are.
func TestTrace_BodyLinesAreMaskedInEveryCarrier(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defPem, defB64)
	for name, ids := range map[string][]string{
		"block not selected": {"err-line", "b64-run"},
		"block selected":     {"err-line", "b64-run", "pem-key"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := traceOptions(t)
			opts.Before, opts.After = 4, 3
			ans := mustTrace(t, traceFiles(t, writeLog(t, t.TempDir(), "app.log", keyLog())), cat, pick(t, cat, ids...), opts)
			byKey := map[string]rxtypes.Finding{}
			for _, f := range ans.Findings {
				byKey[fmt.Sprintf("%s:%d", f.Signature, f.StartLine)] = f
			}
			lines := fileLines(keyLog())
			before := byKey["err-line:2"]
			// The BEGIN line is the block's anchor: its match is masked as a value.
			if want := []string{"----************", "[masked, 64 bytes]", "[masked, 64 bytes]"}; !slices.Equal(before.After, want) || !before.Masked {
				t.Errorf("err-line:2 after %q masked %v, want %q", before.After, before.Masked, want)
			}
			after := byKey["err-line:8"]
			want := []string{"[masked, 64 bytes]", "[masked, 64 bytes]", "[masked, 8 bytes]", "[masked, 25 bytes]"}
			if !slices.Equal(after.Before, want) || !slices.Equal(after.After, lines[8:]) || !after.Masked {
				t.Errorf("err-line:8 before %q after %q masked %v, want %q", after.Before, after.After, after.Masked, want)
			}
			if f := byKey["b64-run:4"]; !slices.Equal(f.Lines, []string{"[masked, 64 bytes]"}) || f.Excerpt != "[masked, 64 bytes]" || !f.Masked {
				t.Errorf("b64-run:4 lines %q excerpt %q masked %v", f.Lines, f.Excerpt, f.Masked)
			}
			raw, err := json.Marshal(ans)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range keyLogSecrets {
				if strings.Contains(string(raw), secret) {
					t.Errorf("the answer holds %q:\n%s", secret, raw)
				}
			}
		})
	}
}
