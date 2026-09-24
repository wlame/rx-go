package logchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// mixedCodecChain is a chain of five frozen parts in five storages and
// an active file, every line of the form
// `<time> LINE <global> part=<name> local=<n>`, a minute apart over the
// whole chain. The third part does not end with a newline. It returns
// the directory, the files by name and the chain's order.
func mixedCodecChain(t *testing.T) (string, map[string]chainFile, []string) {
	t.Helper()
	dir := t.TempDir()
	order := []string{"app.log.5.zst", "app.log.4.xz", "app.log.3.bz2", "app.log.2.gz", "app.log.1", "app.log"}
	codecs := map[string]string{
		"app.log.5.zst": compressedcopy.SeekableZstd, "app.log.4.xz": compressedcopy.Xz,
		"app.log.3.bz2": compressedcopy.Bzip2, "app.log.2.gz": compressedcopy.Gzip,
	}
	counts := []int{40, 25, 31, 50, 12, 7}
	files := map[string]chainFile{}
	var list []chainFile
	global, at := 1, chainBase
	for k, name := range order {
		text := timedLines(at, time.Minute, global, counts[k], name)
		if name == "app.log.3.bz2" {
			text = text[:len(text)-1]
		}
		f := chainFile{name: name, text: text, codec: codecs[name], mtime: at.Add(time.Duration(counts[k]) * time.Minute)}
		files[name] = f
		list = append(list, f)
		global += counts[k]
		at = at.Add(time.Duration(counts[k]) * time.Minute)
	}
	writeChainFiles(t, dir, list)
	return dir, files, order
}

// A ready chain of mixed storages: its parts in time order, each part's
// line count the count of its decompressed text, each global start the
// sum of the counts before it, and the chain's line count and times
// those of the concatenation of its parts read as one file. The same
// with every part indexed and read without a scan.
func TestDescribe_ReadyChainOfMixedStorages(t *testing.T) {
	dir, files, order := mixedCodecChain(t)
	cold := describe(t, dir, "app.log", Options{Scan: true})
	resp := cold.Response
	if resp.State != rxtypes.ChainStateReady || len(resp.Reasons) != 0 {
		t.Fatalf("state %s, reasons %+v", resp.State, resp.Reasons)
	}
	if got := partNamesOf(cold); !slices.Equal(got, order) {
		t.Fatalf("order %v, want %v", got, order)
	}
	start := int64(1)
	for k, p := range resp.Parts {
		want := lineCountOf(files[p.Name].text)
		if p.LineCount == nil || *p.LineCount != want {
			t.Fatalf("%s: line_count %v, want %d", p.Name, p.LineCount, want)
		}
		if p.GlobalStart == nil || *p.GlobalStart != start || cold.Starts[k] != start {
			t.Fatalf("%s: global_start %v (Starts %d), want %d", p.Name, p.GlobalStart, cold.Starts[k], start)
		}
		start += want
	}

	concatenated := concatenation(t, dir, files, order)
	lines, tr := singleFileAnswer(t, concatenated)
	if resp.LineCount == nil || *resp.LineCount != lines {
		t.Fatalf("line_count %v, the concatenation has %d", resp.LineCount, lines)
	}
	activeLines := *resp.Parts[len(resp.Parts)-1].LineCount
	if resp.FrozenLineCount == nil || *resp.FrozenLineCount != lines-activeLines {
		t.Fatalf("frozen_line_count %v, want %d", resp.FrozenLineCount, lines-activeLines)
	}
	if *resp.FirstMs != *tr.FirstMs || *resp.LastMs != *tr.LastMs {
		t.Fatalf("first/last %d/%d, the concatenation %d/%d", *resp.FirstMs, *resp.LastMs, *tr.FirstMs, *tr.LastMs)
	}

	storeIndexes(t, dir, order...)
	indexed := describe(t, dir, "app.log", Options{})
	for _, p := range indexed.Response.Parts {
		if !p.IsIndexed {
			t.Fatalf("%s is not indexed", p.Name)
		}
	}
	requireSameDescription(t, "scan and indexed", resp, indexed.Response)
}

// Names that say the opposite of time (log4j2 numbers its newest file
// highest) give the provisional order; the times give the real one, and
// the global starts follow it.
func TestDescribe_OrderFollowsTimeWhenNamesDisagree(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 10, "1")},
		{name: "app.log.2", text: timedLines(chainBase.Add(time.Hour), time.Second, 11, 5, "2")},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 16, 3, "active")},
	})
	if got := partNames(resolveIn(t, dir, "app.log")); !slices.Equal(got, []string{"app.log.2", "app.log.1", "app.log"}) {
		t.Fatalf("provisional order %v", got)
	}
	d := describe(t, dir, "app.log", Options{Scan: true})
	if got := partNamesOf(d); !slices.Equal(got, []string{"app.log.1", "app.log.2", "app.log"}) || d.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("order %v, state %s", got, d.Response.State)
	}
	if !slices.Equal(d.Starts, []int64{1, 11, 16}) {
		t.Fatalf("starts %v", d.Starts)
	}
	// Order maps the time order back to the provisional one.
	if !slices.Equal(d.Order, []int{1, 0, 2}) {
		t.Fatalf("order map %v", d.Order)
	}
}

// overlapChain is a chain whose frozen part ends overlap seconds after
// the active file starts.
func overlapChain(t *testing.T, overlap time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	frozen := timedLines(chainBase, time.Second, 1, 100, "1")
	active := timedLines(chainBase.Add(99*time.Second-overlap), time.Second, 101, 10, "active")
	writeChainFiles(t, dir, []chainFile{{name: "x.log.1", text: frozen}, {name: "x.log", text: active}})
	return dir
}

// Each check that fails gives its reason, and the chain is invalid.
func TestDescribe_Reasons(t *testing.T) {
	t.Run("a part without timestamps", func(t *testing.T) {
		dir := t.TempDir()
		writeChainFiles(t, dir, []chainFile{
			{name: "x.log.2", text: timedLines(chainBase, time.Second, 1, 5, "2")},
			{name: "x.log.1", text: []byte("no time here\nnor here\n")},
			{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 8, 5, "active")},
		})
		d := describe(t, dir, "x.log", Options{Scan: true})
		if d.Response.State != rxtypes.ChainStateInvalid || !slices.Equal(reasonCodes(d), []string{rxtypes.ChainReasonNoTimestamps}) ||
			!slices.Equal(d.Response.Reasons[0].Parts, []string{"x.log.1"}) {
			t.Fatalf("state %s reasons %+v", d.Response.State, d.Response.Reasons)
		}
		if d.Starts != nil || d.Response.Parts[0].GlobalStart != nil || d.Response.LineCount != nil {
			t.Fatalf("an invalid chain has global numbers")
		}
	})
	t.Run("an overlap of 61 s with the default tolerance", func(t *testing.T) {
		d := describe(t, overlapChain(t, 61*time.Second), "x.log", Options{Scan: true})
		r := d.Response.Reasons
		if d.Response.State != rxtypes.ChainStateInvalid || len(r) != 1 || r[0].Code != rxtypes.ChainReasonOverlap ||
			r[0].OverlapMs == nil || *r[0].OverlapMs != 61_000 || !slices.Equal(r[0].Parts, []string{"x.log.1", "x.log"}) {
			t.Fatalf("state %s reasons %+v", d.Response.State, r)
		}
	})
	t.Run("an overlap of 59 s with the default tolerance", func(t *testing.T) {
		d := describe(t, overlapChain(t, 59*time.Second), "x.log", Options{Scan: true})
		if d.Response.State != rxtypes.ChainStateReady {
			t.Fatalf("state %s reasons %+v", d.Response.State, d.Response.Reasons)
		}
	})
	t.Run("an overlap of 1 s with no tolerance", func(t *testing.T) {
		t.Setenv(config.ChainOverlapSecondsSetting.Name, "0")
		d := describe(t, overlapChain(t, time.Second), "x.log", Options{Scan: true})
		if !slices.Equal(reasonCodes(d), []string{rxtypes.ChainReasonOverlap}) {
			t.Fatalf("reasons %+v", d.Response.Reasons)
		}
		touching := describe(t, overlapChain(t, 0), "x.log", Options{Scan: true})
		if touching.Response.State != rxtypes.ChainStateReady {
			t.Fatalf("a part ending where the next starts: %+v", touching.Response.Reasons)
		}
	})
	t.Run("the active file before the last frozen part", func(t *testing.T) {
		dir := t.TempDir()
		writeChainFiles(t, dir, []chainFile{
			{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 1, 5, "1")},
			{name: "x.log", text: timedLines(chainBase, time.Second, 6, 5, "active")},
		})
		d := describe(t, dir, "x.log", Options{Scan: true})
		if !slices.Equal(reasonCodes(d), []string{rxtypes.ChainReasonActiveNotLast}) ||
			!slices.Equal(d.Response.Reasons[0].Parts, []string{"x.log", "x.log.1"}) ||
			!slices.Equal(partNamesOf(d), []string{"x.log", "x.log.1"}) {
			t.Fatalf("reasons %+v, order %v", d.Response.Reasons, partNamesOf(d))
		}
	})
	t.Run("an unreadable part", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its permissions")
		}
		dir := t.TempDir()
		writeChainFiles(t, dir, []chainFile{
			{name: "x.log.1", text: timedLines(chainBase, time.Second, 1, 5, "1")},
			{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "active")},
		})
		// The listing classified the part; it becomes unreadable after.
		c := resolveIn(t, dir, "x.log")
		locked := filepath.Join(dir, "x.log.1")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
		for _, opts := range []Options{{Scan: true}, {}} {
			d, err := Describe(context.Background(), c, opts)
			if err != nil {
				t.Fatalf("scan %v: %v", opts.Scan, err)
			}
			if !slices.Equal(reasonCodes(d), []string{rxtypes.ChainReasonUnreadable}) ||
				!slices.Equal(d.Response.Reasons[0].Parts, []string{"x.log.1"}) {
				t.Fatalf("scan %v: reasons %+v", opts.Scan, d.Response.Reasons)
			}
		}
	})
	t.Run("too many parts", func(t *testing.T) {
		names := []string{"big.log"}
		for n := 1; n <= MaxParts; n++ {
			names = append(names, "big.log."+strconv.Itoa(n))
		}
		c := Group(testDir, fakeEntries(names...), allText)[0]
		counted := countReads(t)
		d, err := Describe(context.Background(), c, Options{Scan: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(reasonCodes(d), []string{rxtypes.ChainReasonTooManyParts}) || len(d.Response.Parts) != MaxParts+1 {
			t.Fatalf("reasons %+v, %d parts", d.Response.Reasons, len(d.Response.Parts))
		}
		if counted.total() != 0 {
			t.Fatalf("a chain of too many parts was read: %+v", counted)
		}
	})
}

// Empty parts and an empty active file add no lines and take no part in
// the checks; a chain of empty parts only is ready with 0 lines.
func TestDescribe_EmptyParts(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.4", text: timedLines(chainBase, time.Second, 1, 5, "4")},
		{name: "x.log.3", text: nil},
		{name: "x.log.2.gz", text: nil, codec: compressedcopy.Gzip},
		{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 4, "1")},
		{name: "x.log", text: nil},
	})
	for _, opts := range []Options{{Scan: true}, {}} {
		if !opts.Scan {
			storeIndexes(t, dir, "x.log.4", "x.log.2.gz", "x.log.1")
		}
		d := describe(t, dir, "x.log", opts)
		if d.Response.State != rxtypes.ChainStateReady {
			t.Fatalf("scan %v: state %s %+v", opts.Scan, d.Response.State, d.Response.Reasons)
		}
		if got := partNamesOf(d); !slices.Equal(got, []string{"x.log.4", "x.log.3", "x.log.2.gz", "x.log.1", "x.log"}) {
			t.Fatalf("order %v", got)
		}
		if !slices.Equal(d.Starts, []int64{1, 6, 6, 6, 10}) || *d.Response.LineCount != 9 || *d.Response.FrozenLineCount != 9 {
			t.Fatalf("starts %v, line_count %v", d.Starts, *d.Response.LineCount)
		}
	}

	onlyEmpty := t.TempDir()
	writeChainFiles(t, onlyEmpty, []chainFile{{name: "e.log.2"}, {name: "e.log.1"}, {name: "e.log"}})
	for _, opts := range []Options{{Scan: true}, {}} {
		d := describe(t, onlyEmpty, "e.log", opts)
		if d.Response.State != rxtypes.ChainStateReady || *d.Response.LineCount != 0 || d.Response.FirstMs != nil {
			t.Fatalf("scan %v: %s", opts.Scan, jsonOf(t, d.Response))
		}
	}
}

// Over HTTP a frozen part without an index is not read: the chain is
// pending and has no global numbers. Once the part is indexed it is
// ready, with the order and counts the scan of the CLI gave.
func TestDescribe_PendingUntilEveryFrozenPartIsIndexed(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.2.gz", text: timedLines(chainBase, time.Second, 1, 30, "2"), codec: compressedcopy.Gzip},
		{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 31, 20, "1")},
		{name: "x.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 51, 10, "active")},
	})
	storeIndexes(t, dir, "x.log.2.gz")
	scanned := describe(t, dir, "x.log", Options{Scan: true})
	pending := describe(t, dir, "x.log", Options{})
	if pending.Response.State != rxtypes.ChainStatePending || pending.Starts != nil {
		t.Fatalf("state %s starts %v", pending.Response.State, pending.Starts)
	}
	for _, p := range pending.Response.Parts {
		if p.GlobalStart != nil {
			t.Fatalf("%s has a global start before the chain is ready", p.Name)
		}
		if p.Name == "x.log.1" && (p.IsIndexed || p.LineCount != nil) {
			t.Fatalf("x.log.1 was read: %+v", p)
		}
	}
	storeIndexes(t, dir, "x.log.1")
	ready := describe(t, dir, "x.log", Options{})
	if ready.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s %+v", ready.Response.State, ready.Response.Reasons)
	}
	requireSameDescription(t, "scan and indexed", scanned.Response, ready.Response)
	// Indexing the active file too fills its count, and nothing else
	// changes.
	storeIndexes(t, dir, "x.log")
	all := describe(t, dir, "x.log", Options{})
	if all.Response.LineCount == nil || *all.Response.LineCount != 60 {
		t.Fatalf("line_count %v", all.Response.LineCount)
	}
	requireSameDescription(t, "scan and all indexed", scanned.Response, all.Response)
}

// dailyChain is a chain of parts that each hold one day, starting on
// the given day offsets from chainBase.
func dailyChain(t *testing.T, days []int) string {
	t.Helper()
	dir := t.TempDir()
	var files []chainFile
	for k, day := range days {
		name := "d.log." + strconv.Itoa(len(days)-k)
		start := chainBase.AddDate(0, 0, day)
		files = append(files, chainFile{name: name, text: timedLines(start, time.Hour, 24*k+1, 24, name)})
	}
	writeChainFiles(t, dir, files)
	return dir
}

// A hole of three days among daily parts is one gap; three parts are
// too few to look for one.
func TestDescribe_Gaps(t *testing.T) {
	d := describe(t, dailyChain(t, []int{0, 1, 2, 5, 6, 7}), "d.log", Options{Scan: true})
	if d.Response.State != rxtypes.ChainStateReady || len(d.Response.Gaps) != 1 {
		t.Fatalf("state %s gaps %+v", d.Response.State, d.Response.Gaps)
	}
	gap := d.Response.Gaps[0]
	if gap.After != "d.log.4" || gap.Before != "d.log.3" ||
		gap.FromMs != chainBase.AddDate(0, 0, 2).Add(23*time.Hour).UnixMilli() || gap.ToMs != chainBase.AddDate(0, 0, 5).UnixMilli() {
		t.Fatalf("gap %+v", gap)
	}
	if few := describe(t, dailyChain(t, []int{0, 1, 5}), "d.log", Options{Scan: true}); len(few.Response.Gaps) != 0 {
		t.Fatalf("three parts: gaps %+v", few.Response.Gaps)
	}
	if steady := describe(t, dailyChain(t, []int{0, 1, 2, 3, 4, 5}), "d.log", Options{Scan: true}); len(steady.Response.Gaps) != 0 {
		t.Fatalf("no hole: gaps %+v", steady.Response.Gaps)
	}
}

// Locate maps a global line to its part and local line, over empty
// parts and up to the active file, which takes every line past the
// frozen parts.
func TestDescription_Locate(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.3", text: timedLines(chainBase, time.Second, 1, 5, "3")},
		{name: "x.log.2"},
		{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 4, "1")},
		{name: "x.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 10, 2, "active")},
	})
	d := describe(t, dir, "x.log", Options{Scan: true})
	cases := []struct {
		global int64
		part   string
		local  int64
		ok     bool
	}{
		{0, "", 0, false}, {1, "x.log.3", 1, true}, {5, "x.log.3", 5, true}, {6, "x.log.1", 1, true},
		{9, "x.log.1", 4, true}, {10, "x.log", 1, true}, {11, "x.log", 2, true}, {50, "x.log", 41, true},
	}
	for _, tc := range cases {
		part, local, ok := d.Locate(tc.global)
		if ok != tc.ok || (ok && (d.Response.Parts[part].Name != tc.part || local != tc.local)) {
			t.Errorf("Locate(%d) = %d %d %v, want %s %d %v", tc.global, part, local, ok, tc.part, tc.local, tc.ok)
		}
	}
	// Without an active file, a line past the end is in no part.
	if err := os.Remove(filepath.Join(dir, "x.log")); err != nil {
		t.Fatal(err)
	}
	frozen := describe(t, dir, "x.log", Options{Scan: true})
	if _, _, ok := frozen.Locate(10); ok {
		t.Fatal("line 10 of a chain of 9 lines located")
	}
	if part, local, ok := frozen.Locate(9); !ok || frozen.Response.Parts[part].Name != "x.log.1" || local != 4 {
		t.Fatalf("Locate(9) = %d %d %v", part, local, ok)
	}
	pending := describe(t, dir, "x.log", Options{})
	if _, _, ok := pending.Locate(1); ok {
		t.Fatal("a pending chain located a line")
	}
}

// A part renamed or replaced between the listing and the read is a
// changed part, whether the part is read by a scan or not.
func TestDescribe_APartChangedAfterTheListing(t *testing.T) {
	for _, opts := range []Options{{Scan: true}, {}} {
		dir := t.TempDir()
		writeChainFiles(t, dir, []chainFile{
			{name: "x.log.1", text: timedLines(chainBase, time.Second, 1, 5, "1")},
			{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "active")},
		})
		c := resolveIn(t, dir, "x.log")
		// A rotation: x.log.1 is replaced by another file of that name.
		// The new file exists before the old one goes, so it cannot
		// reuse the old one's inode.
		fresh := filepath.Join(dir, "x.log.1.new")
		if err := os.WriteFile(fresh, timedLines(chainBase, time.Second, 1, 9, "other"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fresh, filepath.Join(dir, "x.log.1")); err != nil {
			t.Fatal(err)
		}
		_, err := Describe(context.Background(), c, opts)
		if !errors.Is(err, ErrPartChanged) || !errors.Is(err, paths.ErrFileChanged) {
			t.Fatalf("scan %v: %v, want ErrPartChanged", opts.Scan, err)
		}
	}
}

// A scan builds a part's index through a pin of its own. When the
// part's name led to another file by then, the index describes that
// file, not the one the listing pinned, and the part is a changed part:
// it is never read as the listed one.
func TestDescribe_AScanOfAnotherFileIsAChangedPart(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.1", text: timedLines(chainBase, time.Second, 1, 5, "1")},
		{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 6, 5, "active")},
	})
	other := writeChainFiles(t, t.TempDir(), []chainFile{{name: "other.log", text: timedLines(chainBase, time.Second, 1, 9, "other")}})
	c := resolveIn(t, dir, "x.log")
	build := buildPartIndex
	t.Cleanup(func() { buildPartIndex = build })
	// The race, made certain: every build reads the other file.
	buildPartIndex = func(string) (*rxtypes.UnifiedFileIndex, error) { return build(filepath.Join(other, "other.log")) }
	if _, err := Describe(context.Background(), c, Options{Scan: true}); !errors.Is(err, ErrPartChanged) {
		t.Fatalf("describe = %v, want ErrPartChanged", err)
	}
}
