package samples

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/index"
)

// boundsText is a log of 40 lines a minute apart from 2026-10-01 10:00,
// with a line without a timestamp after every fourth.
func boundsText() []byte {
	var buf bytes.Buffer
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for n := 1; n <= 40; n++ {
		fmt.Fprintf(&buf, "%s LINE %d\n", at.Add(time.Duration(n)*time.Minute).Format("2006-01-02 15:04:05.000"), n)
		if n%4 == 0 {
			buf.WriteString("    continued\n")
		}
	}
	return buf.Bytes()
}

// A time query resolved once (ResolveQueries) and searched as a bound
// (Request.TimeBounds) finds the line the query itself finds in the
// file, in every storage, with an index and without.
func TestTimeBounds_FindTheLinesTheQueriesFind(t *testing.T) {
	text := boundsText()
	dir := t.TempDir()
	files := compressedCopiesOf(t, text, dir)
	files["app.log"] = filepath.Join(dir, "app.log")
	if err := os.WriteFile(files["app.log"], text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	queries := []string{"2026-10-01 10:07:30", "2026-10-01 09:00", "2026-10-01 11:00", "10:20", "2026-10-01T10:31:00Z"}
	// The file's own first and last timestamps, 10:01 and 10:40 UTC: a
	// time of day takes its date from them, as it does in the file.
	first := time.Date(2026, 10, 1, 10, 1, 0, 0, time.UTC).UnixMilli()
	last := time.Date(2026, 10, 1, 10, 40, 0, 0, time.UTC).UnixMilli()
	scope := QueryScope{FirstMs: &first, LastMs: &last}
	// Cold first, then with the index stored.
	ways := []struct {
		loader IndexLoader
		store  bool
	}{{NoIndex, false}, {StoredIndex, true}}
	for name, path := range files {
		for _, way := range ways {
			if way.store {
				storeIndex(t, path)
			}
			loader := way.loader
			single, err := Resolve(t.Context(), Request{Path: path, Timestamps: queries, IndexLoader: loader})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			resolved, err := ResolveQueries(t.Context(), Request{Path: path, IndexLoader: loader}, queries, scope)
			if err != nil {
				t.Fatalf("%s: resolve: %v", name, err)
			}
			bounds := map[string]TimeBound{}
			for i, q := range resolved {
				if q.Range || q.Start == nil {
					t.Fatalf("%s: query %d resolved as %+v", name, i, q)
				}
				bounds[q.Value] = *q.Start
			}
			bounded, err := Resolve(t.Context(), Request{Path: path, Timestamps: queries, TimeBounds: bounds, IndexLoader: loader})
			if err != nil {
				t.Fatalf("%s: bounded: %v", name, err)
			}
			for _, q := range queries {
				if single.Timestamps[q] != bounded.Timestamps[q] {
					t.Fatalf("%s: %q found line %d as a query, %d as a bound", name, q, single.Timestamps[q], bounded.Timestamps[q])
				}
			}
		}
	}
}

// A value of Timestamps without a bound in TimeBounds is a caller's
// defect, refused rather than parsed.
func TestTimeBounds_AValueWithoutABoundIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, boundsText(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Resolve(t.Context(), Request{Path: path, Timestamps: []string{"10:20"}, TimeBounds: map[string]TimeBound{}, IndexLoader: NoIndex})
	if err == nil || !strings.Contains(err.Error(), "no resolved bound") {
		t.Fatalf("got %v", err)
	}
}

// EndsWithLineBreak reads the last byte of the text in every storage.
func TestEndsWithLineBreak(t *testing.T) {
	for _, withBreak := range []bool{true, false} {
		text := boundsText()
		if !withBreak {
			text = text[:len(text)-1]
		}
		dir := t.TempDir()
		files := compressedCopiesOf(t, text, dir)
		files["app.log"] = filepath.Join(dir, "app.log")
		if err := os.WriteFile(files["app.log"], text, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		for name, path := range files {
			got, err := EndsWithLineBreak(context.Background(), Request{Path: path, IndexLoader: NoIndex}, int64(len(text)))
			if err != nil || got != withBreak {
				t.Fatalf("%s (break %v): %v, %v", name, withBreak, got, err)
			}
		}
	}
}

// fakeEarlier is an Earlier that answers with one stamp and records how
// it was called.
type fakeEarlier struct {
	stamp    EarlierStamp
	settled  EarlierStamp
	calls    int
	settles  int
	headSeen bool
}

func (f *fakeEarlier) LastStamp(ctx context.Context, _ int64) (EarlierStamp, error) {
	f.calls++
	_, f.headSeen = headLimitOf(ctx)
	return f.stamp, nil
}

func (f *fakeEarlier) Settle(context.Context, EarlierStamp) (EarlierStamp, error) {
	f.settles++
	return f.settled, nil
}

// A sample whose first lines have no timestamp of their own carries the
// earlier text's stamp across the file's first byte, while it lies
// within RX_TIMESTAMP_LOOKBACK_KB; a stamp inside the file wins; the
// earlier text is asked once per answer and never under the file's head
// limit; and a line exactly at the distance settles the stamp first.
func TestLineTimestamps_TheEarlierTextCarriesAcrossTheFilesStart(t *testing.T) {
	t.Setenv("RX_TIMESTAMP_LOOKBACK_KB", "1")
	text := []byte("    head one\n    head two\n" + string(boundsText()))
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	one, two := int64(1)<<40, int64(1)<<41
	earlier := &fakeEarlier{stamp: EarlierStamp{Found: true, Start: -1000, Instant: one, InstantOK: true}}
	resp, err := Resolve(t.Context(), Request{Path: path, Lines: lineRanges(t, "1-3,2"), Earlier: earlier, IndexLoader: NoIndex})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Line 1 starts at 0 (1000 bytes after the stamp), line 2 at 13
	// (1013): both carry it; line 3 has its own.
	if got := stampWords(resp.LineTimestamps["1-3"]); got[0] != fmt.Sprint(one) || got[1] != fmt.Sprint(one) || got[2] == fmt.Sprint(one) {
		t.Fatalf("1-3: %v", got)
	}
	if got := stampWords(resp.LineTimestamps["2"]); got[0] != fmt.Sprint(one) {
		t.Fatalf("2: %v", got)
	}
	if earlier.calls != 1 {
		t.Fatalf("the earlier text was asked %d times, want once", earlier.calls)
	}

	// Line 2 starts exactly 1024 bytes after a stamp that may be one
	// byte earlier: the stamp is settled, and the settled one decides.
	earlier = &fakeEarlier{
		stamp:   EarlierStamp{Found: true, Start: -1011, OneEarlier: true, Instant: one, InstantOK: true},
		settled: EarlierStamp{Found: true, Start: -1012, Instant: two, InstantOK: true},
	}
	resp, _, err = ResolveFromHead(t.Context(), Request{Path: path, Lines: lineRanges(t, "1-2"), Earlier: earlier}, 1<<20)
	if err != nil {
		t.Fatalf("resolve from the head: %v", err)
	}
	if got := stampWords(resp.LineTimestamps["1-2"]); got[0] != fmt.Sprint(two) || got[1] != "null" || earlier.settles != 1 {
		t.Fatalf("at exactly the distance: %v, %d settles", got, earlier.settles)
	}
	if earlier.headSeen {
		t.Fatalf("the earlier text was asked under the file's head limit")
	}

	// A file whose own lines answer never asks.
	earlier = &fakeEarlier{stamp: EarlierStamp{Found: true, Start: -10, Instant: one, InstantOK: true}}
	if _, err := Resolve(t.Context(), Request{Path: path, Lines: lineRanges(t, "4-6"), Earlier: earlier, IndexLoader: NoIndex}); err != nil || earlier.calls != 0 {
		t.Fatalf("a sample after a stamped line asked %d times (%v)", earlier.calls, err)
	}
}

// storeIndex builds and stores the line index of path, as rx index
// does.
func storeIndex(t *testing.T, path string) {
	t.Helper()
	idx, err := index.Build(path, index.BuildOptions{StepBytes: 256})
	if err != nil {
		t.Fatalf("index %s: %v", path, err)
	}
	if _, err := index.Save(idx); err != nil {
		t.Fatalf("save the index of %s: %v", path, err)
	}
}

// lineRanges parses a lines spec.
func lineRanges(t *testing.T, spec string) []OffsetOrRange {
	t.Helper()
	parsed, err := ParseCSV(spec)
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	return parsed
}

// stampWords writes line timestamps as words, "null" for none.
func stampWords(stamps []*int64) []string {
	out := make([]string, len(stamps))
	for i, s := range stamps {
		out[i] = "null"
		if s != nil {
			out[i] = fmt.Sprint(*s)
		}
	}
	return out
}
