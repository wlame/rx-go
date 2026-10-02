package findings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// scanFor scans the file at path for ids and returns the file and the
// answer.
func scanFor(t *testing.T, path string, cat *signatures.Catalogue, ids ...string) (trace.SearchFile, *Answer) {
	t.Helper()
	file := classify(t, path)
	ans, err := Scan(context.Background(), file, cat, pick(t, cat, ids...), testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	return file, ans
}

// zoneOf parses a zone name as file_tz takes it.
func zoneOf(t *testing.T, name string) config.Zone {
	t.Helper()
	zone, err := config.ParseZone(name)
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

// WireFindings gives every stored finding of the selection in the one
// order (first line, last line, signature id), with no file id for an
// answer about one file, and the density series of the selection.
func TestWireFindings_EveryStoredFindingInTheOneOrder(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defWarnLine, defTrace, defBurst)
	file, ans := scanFor(t, writeLog(t, t.TempDir(), "app.log", sampleLog()), cat, "warn-line", "err-line", "py-trace", "line-burst")
	got, series, err := WireFindings(ans, file, pick(t, cat, "warn-line", "err-line", "py-trace", "line-burst"), "", traceOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, id := range []string{"warn-line", "err-line", "py-trace", "line-burst"} {
		want += len(ans.BySignature[id].Findings)
	}
	if len(got) != want || want == 0 {
		t.Fatalf("%d findings, want %d", len(got), want)
	}
	for i, f := range got {
		if f.File != "" {
			t.Fatalf("finding %d has file id %q", i, f.File)
		}
		if i > 0 && rxtypes.CompareFindings(&got[i-1], &f) > 0 {
			t.Fatalf("findings %d and %d out of order: %+v %+v", i-1, i, got[i-1], f)
		}
	}
	burst := ans.BySignature["line-burst"].Series
	if s, ok := series["line-burst"]; !ok || burst == nil || s.BucketMS != burst.BucketMS || s.StartMS != burst.StartMS ||
		!slices.Equal(s.Counts, burst.Counts) {
		t.Fatalf("series %+v, want the scan's %+v", series, burst)
	}
	if len(series) != 1 {
		t.Fatalf("series of %v, want line-burst only", series)
	}
}

// Under a file zone, a zone-less file's times are its wall clocks read
// in that zone, the instants line_timestamps gives; a file whose
// timestamps carry zones reads each line's written wall clock in the
// zone when its line index records where the offset changes, and has
// no time without it (it cannot be known without reading the file).
func TestWireFindings_FileZoneReadsTheTimesAsSamplesDoes(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	cat := loadCatalogue(t, defErrLine)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	texts := map[string]string{
		"local.log": "2026-01-15 10:00:00 INFO start\n2026-01-15 10:00:05 ERROR boom\nERROR no stamp\n",
		"zoned.log": "2026-01-15T10:00:00+01:00 INFO start\n2026-01-15T10:00:05+02:00 ERROR boom\nERROR no stamp\n",
	}
	for name, text := range texts {
		path := writeLog(t, dir, name, text)
		file, ans := scanFor(t, path, cat, "err-line")
		idx, err := index.Build(path, index.BuildOptions{StepBytes: 16})
		if err != nil {
			t.Fatal(err)
		}
		for _, withIndex := range []bool{false, true} {
			opts := traceOptions(t)
			opts.FileZone = tokyo
			opts.IndexLoader = samples.NoIndex
			if withIndex {
				opts.IndexLoader = func(string) (*rxtypes.UnifiedFileIndex, error) { return idx, nil }
			}
			got, _, err := WireFindings(ans, file, pick(t, cat, "err-line"), "", opts)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := samples.Resolve(context.Background(), samples.Request{
				Path: path, Lines: []samples.OffsetOrRange{{Start: 2}, {Start: 3}}, IndexLoader: opts.IndexLoader,
				FileZone: zoneOf(t, "Asia/Tokyo"),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range got {
				want := resp.LineTimestamps[strconv.FormatInt(f.StartLine, 10)]
				unknown := name == "zoned.log" && !withIndex
				switch {
				case unknown && f.TimeMS != nil:
					t.Errorf("%s line %d without an index: time %d, want null", name, f.StartLine, *f.TimeMS)
				case !unknown && (f.TimeMS == nil || len(want) != 1 || want[0] == nil || *f.TimeMS != *want[0]):
					t.Errorf("%s line %d index=%v: time %v, want line_timestamps %v", name, f.StartLine, withIndex, f.TimeMS, want)
				}
			}
		}
	}
}

// FillText gives a page of findings its text as a trace answer does:
// each line masked by every loaded redacting signature, every body line
// of a redacting block shown as its length (here the block is not even
// selected), the lines asked for before each finding, and a budget that
// cuts the text and marks the rest.
func TestFillText_APageOfFindingsIsMaskedLikeATraceAnswer(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defPem)
	path := writeLog(t, t.TempDir(), "app.log", keyLog())
	file, ans := scanFor(t, path, cat, "err-line")
	all, _, err := WireFindings(ans, file, pick(t, cat, "err-line"), "", traceOptions(t))
	if err != nil || len(all) != 2 {
		t.Fatalf("findings %+v, %v", all, err)
	}
	opts := traceOptions(t)
	opts.Before = 4
	page := slices.Clone(all[1:]) // the error after the key, line 8
	if err := FillText(context.Background(), file, page, ans, cat, opts); err != nil {
		t.Fatal(err)
	}
	f := page[0]
	wantBefore := []string{"[masked, 64 bytes]", "[masked, 64 bytes]", "[masked, 8 bytes]", "[masked, 25 bytes]"}
	if !slices.Equal(f.Before, wantBefore) || !slices.Equal(f.Lines, []string{"2026-10-10 14:00:02.000 ERROR after the key"}) || !f.Masked {
		t.Fatalf("before %q lines %q masked %v, want before %q", f.Before, f.Lines, f.Masked, wantBefore)
	}
	raw, _ := json.Marshal(page)
	for _, secret := range keyLogSecrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the page holds a key line: %s", raw)
		}
	}

	// A budget that holds the first finding's text only.
	opts = traceOptions(t)
	opts.TextBudget = int64(len("2026-10-10 14:00:01.000 ERROR before the key") + 1)
	page = slices.Clone(all)
	if err := FillText(context.Background(), file, page, ans, cat, opts); err != nil {
		t.Fatal(err)
	}
	if len(page[0].Lines) != 1 || page[0].LinesTruncated || page[1].Lines != nil || !page[1].LinesTruncated {
		t.Fatalf("budget: %+v", page)
	}
}

// A page in descending order (the findings before a line) is read in
// line order and keeps its own order.
func TestFillText_DescendingPageKeepsItsOrder(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	path := writeLog(t, t.TempDir(), "app.log", "ERROR one\nok\nERROR three\n")
	file, ans := scanFor(t, path, cat, "err-line")
	all, _, err := WireFindings(ans, file, pick(t, cat, "err-line"), "", traceOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	page := []rxtypes.Finding{all[1], all[0]}
	opts := traceOptions(t)
	opts.Before = 1
	if err := FillText(context.Background(), file, page, ans, cat, opts); err != nil {
		t.Fatal(err)
	}
	if page[0].StartLine != 3 || !slices.Equal(page[0].Lines, []string{"ERROR three"}) || !slices.Equal(page[0].Before, []string{"ok"}) ||
		page[1].StartLine != 1 || !slices.Equal(page[1].Lines, []string{"ERROR one"}) || len(page[1].Before) != 0 {
		t.Fatalf("page %+v", page)
	}
}

// FillText refuses an answer that holds findings without the end of the
// text its scan read: reading without that end could show lines no
// masked range covers.
func TestFillText_RefusesAnAnswerWithoutItsScannedEnd(t *testing.T) {
	cat := loadCatalogue(t, defErrLine)
	path := writeLog(t, t.TempDir(), "app.log", "ERROR one\n")
	file, ans := scanFor(t, path, cat, "err-line")
	all, _, err := WireFindings(ans, file, pick(t, cat, "err-line"), "", traceOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	ans.TextLines, ans.TextBytes = 0, 0
	if err := FillText(context.Background(), file, all, ans, cat, traceOptions(t)); !errors.Is(err, ErrNoScannedEnd) {
		t.Fatalf("FillText = %v, want ErrNoScannedEnd", err)
	}
}

// Cached counts the selected signatures an answer took from the cache:
// none for a first scan, the ones already there for a scan of more,
// every one for a scan the cache answers whole and for a read, none
// with force or without the cache.
func TestAnswer_CachedCountsTheSignaturesTheCacheAnswered(t *testing.T) {
	cat := loadCatalogue(t, defErrLine, defWarnLine)
	file := classify(t, writeLog(t, t.TempDir(), "app.log", "ERROR one\nWARN two\n"))
	opts := testOptions(t)
	scan := func(o ScanOptions, ids ...string) int {
		t.Helper()
		ans, err := Scan(context.Background(), file, cat, pick(t, cat, ids...), o)
		if err != nil {
			t.Fatal(err)
		}
		return ans.Cached
	}
	if got := scan(opts, "err-line"); got != 0 {
		t.Errorf("first scan: cached %d, want 0", got)
	}
	if got := scan(opts, "err-line", "warn-line"); got != 1 {
		t.Errorf("scan of one more: cached %d, want 1", got)
	}
	if got := scan(opts, "err-line", "warn-line"); got != 2 {
		t.Errorf("scan the cache answers: cached %d, want 2", got)
	}
	forced := opts
	forced.Force = true
	if got := scan(forced, "err-line", "warn-line"); got != 0 {
		t.Errorf("forced scan: cached %d, want 0", got)
	}
	noCache := opts
	noCache.NoCache = true
	if got := scan(noCache, "err-line"); got != 0 {
		t.Errorf("scan without the cache: cached %d, want 0", got)
	}
	read, err := Read(file, cat, pick(t, cat, "err-line", "warn-line"), opts)
	if err != nil || read.Cached != 2 {
		t.Errorf("read: cached %d, %v; want 2", read.Cached, err)
	}
}
