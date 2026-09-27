package logchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/testutil/compressedcopy"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/internal/trace"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// searchFor searches paths for patterns as a chain search, without
// search roots, and fails the test on an error.
func searchFor(t *testing.T, req SearchRequest) *SearchResult {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Fatalf("ripgrep is required: %v", err)
	}
	paths.Reset()
	res, err := Search(context.Background(), trace.New(), req)
	if err != nil {
		t.Fatalf("search %v for %v: %v", req.Paths, req.Patterns, err)
	}
	return res
}

// foundLine is one match as the tests compare matches: the file it is
// in, its offset, its line in that file, its line in its chain and its
// text.
type foundLine struct {
	path            string
	offset          int64
	line, chainLine int64
	text            string
}

// foundLines lists the matches of an answer in its order.
func foundLines(resp *rxtypes.ChainTraceResponse) []foundLine {
	out := make([]foundLine, 0, len(resp.Matches))
	for _, m := range resp.Matches {
		out = append(out, foundLine{
			path: resp.Files[m.File], offset: m.Offset, line: int64(m.AbsoluteLineNumber),
			chainLine: m.ChainLine, text: *m.LineText,
		})
	}
	return out
}

// globalOf is the global number a test line names: `… LINE <n> …`.
var globalOf = regexp.MustCompile(`LINE (\d+) `)

// The owner's rule for search: one pattern over a chain by its handle,
// by its directory, and with `rx trace` on each part gives one match
// set; each chain_line is the part's start plus the part's line number,
// which is the line `rx trace` gives on the concatenation of the parts;
// each offset is the line's place in the part's decompressed text. Cold
// (the chain described by a scan, as `rx logs trace` does) and with
// every part indexed (as over HTTP), the answers are the same.
func TestSearch_AChainByItsHandleByItsDirectoryAndPartByPart(t *testing.T) {
	dir, files, order := mixedCodecChain(t)
	concat := concatenation(t, t.TempDir(), files, order)
	patterns := []string{`LINE [0-9]*7 `}

	byHandle := searchFor(t, SearchRequest{Paths: []string{filepath.Join(dir, "app.log")}, Patterns: patterns, Scan: true})
	byDir := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns, Scan: true})
	got := foundLines(byHandle.Answer)
	if dirLines := foundLines(byDir.Answer); !slices.Equal(got, dirLines) {
		t.Fatalf("by handle %v\nby directory %v", got, dirLines)
	}
	if len(got) == 0 {
		t.Fatal("no match")
	}
	for _, answer := range []*rxtypes.ChainTraceResponse{byHandle.Answer, byDir.Answer} {
		ref, ok := answer.Chains["c1"]
		if len(answer.Chains) != 1 || !ok || ref.State != rxtypes.ChainStateReady || ref.Path != filepath.Join(dir, "app.log") {
			t.Fatalf("chains %+v", answer.Chains)
		}
		for k, id := range ref.Parts {
			if answer.Files[id] != filepath.Join(dir, order[k]) || id != "f"+strconv.Itoa(k+1) {
				t.Fatalf("part %d is %s = %s, want %s", k, id, answer.Files[id], order[k])
			}
		}
	}

	// rx trace on each part: the same lines at the same offsets and
	// line numbers, and each offset the line's place in the part's text.
	var perPart []foundLine
	for _, name := range order {
		path := filepath.Join(dir, name)
		resp, err := trace.New().RunWithOptions(context.Background(), []string{path}, patterns, trace.Options{})
		if err != nil {
			t.Fatalf("trace %s: %v", name, err)
		}
		text := files[name].text
		for _, m := range resp.Matches {
			line := strings.TrimSuffix(*m.LineText, "\n")
			if at := bytes.Index(text, []byte(line)); int64(at) != m.Offset {
				t.Fatalf("%s: offset %d, the line is at %d of the part's text", name, m.Offset, at)
			}
			perPart = append(perPart, foundLine{path: path, offset: m.Offset, line: int64(m.AbsoluteLineNumber), text: *m.LineText})
		}
	}
	// rx trace on the concatenation numbers each line globally.
	whole, err := trace.New().RunWithOptions(context.Background(), []string{concat}, patterns, trace.Options{})
	if err != nil {
		t.Fatalf("trace the concatenation: %v", err)
	}
	globalByText := map[string]int64{}
	for _, m := range whole.Matches {
		globalByText[strings.TrimSuffix(*m.LineText, "\n")] = int64(m.AbsoluteLineNumber)
	}
	if len(perPart) != len(got) || len(whole.Matches) != len(got) {
		t.Fatalf("%d matches by chain, %d part by part, %d in the concatenation", len(got), len(perPart), len(whole.Matches))
	}
	for i, m := range got {
		want := perPart[i]
		want.chainLine = m.chainLine
		if m != want {
			t.Fatalf("match %d: %+v, part by part %+v", i, m, want)
		}
		named, _ := strconv.ParseInt(globalOf.FindStringSubmatch(m.text)[1], 10, 64)
		inConcat := globalByText[strings.TrimSuffix(m.text, "\n")]
		if m.chainLine != inConcat || m.chainLine != named {
			t.Fatalf("match %d chain_line %d, the concatenation gives %d, the line names %d", i, m.chainLine, inConcat, named)
		}
	}

	storeIndexes(t, dir, order...)
	indexed := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns})
	if again := foundLines(indexed.Answer); !slices.Equal(again, got) {
		t.Fatalf("indexed %v\ncold %v", again, got)
	}
}

// fileReads records the files a search reads, through the OnFile hook.
type fileReads struct {
	trace.NoopHookFirer
	mu    sync.Mutex
	paths []string
}

func (f *fileReads) OnFile(_ context.Context, path string, _ trace.FileInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, path)
}

// twelvePartChain writes a chain of twelve parts, x.log.11.gz (oldest)
// to x.log.1 and x.log, each with one line both patterns of
// bothPatterns match, and returns its directory and its parts in order.
func twelvePartChain(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var list []chainFile
	var order []string
	for k := 11; k >= 0; k-- {
		name := "x.log." + strconv.Itoa(k)
		codec := compressedcopy.Gzip
		switch k {
		case 0:
			name, codec = "x.log", ""
		case 1:
			codec = ""
		default:
			name += ".gz"
		}
		at := chainBase.Add(time.Duration(11-k) * time.Hour)
		text := fmt.Sprintf("%s quiet part=%s\n%s alpha beta part=%s\n%s quiet again\n",
			at.Format("2006-01-02 15:04:05"), name, at.Add(time.Second).Format("2006-01-02 15:04:05"), name,
			at.Add(2*time.Second).Format("2006-01-02 15:04:05"))
		list = append(list, chainFile{name: name, text: []byte(text), codec: codec, mtime: at})
		order = append(order, name)
	}
	writeChainFiles(t, dir, list)
	return dir, order
}

// bothPatterns match the one marked line of each part of
// twelvePartChain, so that line is two matches.
var bothPatterns = []string{"alpha", "beta"}

// Matches come in the parts' order, past nine parts (f2 before f10).
// With max_results=12 every part is read, one matched line each, and
// the cut keeps the matches of the six oldest parts.
func TestSearch_MatchesFollowThePartsOrderAndTheCutKeepsTheFirstParts(t *testing.T) {
	dir, order := twelvePartChain(t)
	full := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: bothPatterns, Scan: true})
	var gotParts []string
	for _, m := range full.Answer.Matches {
		gotParts = append(gotParts, filepath.Base(full.Answer.Files[m.File]))
	}
	var want []string
	for _, name := range order {
		want = append(want, name, name)
	}
	if !slices.Equal(gotParts, want) {
		t.Fatalf("matches come from %v, want %v", gotParts, want)
	}

	reads := &fileReads{}
	limit := 12
	capped := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: bothPatterns, Scan: true,
		Options: trace.Options{MaxResults: &limit, HookFirer: reads}})
	if len(reads.paths) != len(order) {
		t.Fatalf("the capped search read %d parts, want all %d: %v", len(reads.paths), len(order), reads.paths)
	}
	gotParts = gotParts[:0]
	for _, m := range capped.Answer.Matches {
		gotParts = append(gotParts, filepath.Base(capped.Answer.Files[m.File]))
	}
	if !slices.Equal(gotParts, want[:limit]) {
		t.Fatalf("the cut kept %v, want the first six parts' %v", gotParts, want[:limit])
	}
}

// A directory with two chains and three files of their own: two
// entries in chains, in the order the walk meets them, each chain's
// parts together in its order where the walk met its first file, and
// the other files where the walk lists them, without a chain.
func TestSearch_ADirectoryWithTwoChainsAndThreeFiles(t *testing.T) {
	dir := t.TempDir()
	line := func(at time.Time, what string) []byte {
		return []byte(at.Format("2006-01-02 15:04:05") + " hit " + what + "\n")
	}
	writeChainFiles(t, dir, []chainFile{
		{name: "a.log.2.gz", text: line(chainBase, "a2"), codec: compressedcopy.Gzip},
		{name: "a.log.1", text: line(chainBase.Add(time.Hour), "a1")},
		{name: "a.log", text: line(chainBase.Add(2*time.Hour), "a")},
		{name: "b.log-20261001.gz", text: line(chainBase, "b1"), codec: compressedcopy.Gzip},
		{name: "b.log-20261002.gz", text: line(chainBase.Add(24*time.Hour), "b2"), codec: compressedcopy.Gzip},
		{name: "notes.txt", text: []byte("hit notes\n")},
		{name: "other.log", text: []byte("hit other\n")},
		{name: "zz.txt", text: []byte("hit zz\n")},
	})
	res := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: []string{"hit"}, Scan: true})
	answer := res.Answer

	wantFiles := []string{"a.log.2.gz", "a.log.1", "a.log", "b.log-20261001.gz", "b.log-20261002.gz", "notes.txt", "other.log", "zz.txt"}
	for i, name := range wantFiles {
		if id := "f" + strconv.Itoa(i+1); answer.Files[id] != filepath.Join(dir, name) {
			t.Fatalf("files[%s] = %s, want %s", id, answer.Files[id], name)
		}
	}
	wantChains := map[string]rxtypes.ChainRef{
		"c1": {Path: filepath.Join(dir, "a.log"), Name: "a.log", Parts: []string{"f1", "f2", "f3"}, State: rxtypes.ChainStateReady},
		"c2": {Path: filepath.Join(dir, "b.log"), Name: "b.log", Parts: []string{"f4", "f5"}, State: rxtypes.ChainStateReady},
	}
	if len(answer.Chains) != len(wantChains) {
		t.Fatalf("chains %+v", answer.Chains)
	}
	for id, want := range wantChains {
		got := answer.Chains[id]
		if got.Path != want.Path || got.Name != want.Name || !slices.Equal(got.Parts, want.Parts) || got.State != want.State ||
			len(got.Fingerprint) != 16 || len(got.Reasons) != 0 {
			t.Fatalf("chains[%s] = %+v, want %+v", id, got, want)
		}
	}
	if !slices.Equal(answer.ScannedFiles, res.Trace.ScannedFiles) || len(answer.ScannedFiles) != len(wantFiles) {
		t.Fatalf("scanned_files %v", answer.ScannedFiles)
	}
	wantChainLines := []int64{1, 2, 3, 1, 2, -1, -1, -1}
	for i, m := range answer.Matches {
		inChain := i < 5
		if (m.Chain != nil) != inChain || m.ChainLine != wantChainLines[i] {
			t.Fatalf("match %d (%s): chain %v, chain_line %d", i, answer.Files[m.File], m.Chain, m.ChainLine)
		}
	}
	if *answer.Matches[0].Chain != "c1" || *answer.Matches[3].Chain != "c2" {
		t.Fatalf("matches name chains %s and %s", *answer.Matches[0].Chain, *answer.Matches[3].Chain)
	}
}

// One generation in two encodings is one part: the plain one is
// searched, the other is skipped with duplicate_part, by the handle and
// by the directory.
func TestSearch_OtherEncodingsOfAPartAreSkipped(t *testing.T) {
	dir := t.TempDir()
	first := timedLines(chainBase, time.Second, 1, 3, "1")
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.2.gz", text: timedLines(chainBase.Add(-time.Hour), time.Second, 1, 3, "2"), codec: compressedcopy.Gzip},
		{name: "x.log.1", text: first},
		{name: "x.log.1.gz", text: first, codec: compressedcopy.Gzip},
		{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 7, 3, "active")},
	})
	for _, p := range []string{dir, filepath.Join(dir, "x.log")} {
		answer := searchFor(t, SearchRequest{Paths: []string{p}, Patterns: []string{"LINE"}, Scan: true}).Answer
		duplicate := filepath.Join(dir, "x.log.1.gz")
		if !slices.Contains(answer.SkippedFiles, duplicate) {
			t.Fatalf("%s: skipped %v, want %s", p, answer.SkippedFiles, duplicate)
		}
		for _, skip := range answer.SkipReasons {
			if skip.Path == duplicate && !strings.HasPrefix(skip.Reason, ReasonDuplicatePart+":") {
				t.Fatalf("%s: reason %q", p, skip.Reason)
			}
		}
		for _, path := range answer.Files {
			if path == duplicate {
				t.Fatalf("%s: the duplicate is searched: %v", p, answer.Files)
			}
		}
		if len(answer.Matches) != 9 || len(answer.Chains["c1"].Parts) != 3 {
			t.Fatalf("%s: %d matches, chains %+v", p, len(answer.Matches), answer.Chains)
		}
	}
}

// A pending chain (a frozen part without a line index, over HTTP) is
// searched in its provisional order; its matches have their lines in
// their parts and chain_line -1. Once the parts are indexed the chain
// is ready and every chain_line is set; the two answers agree under
// the -1 rule, and the trace answers are the same.
func TestSearch_APendingChainHasNoChainLinesUntilItsPartsAreIndexed(t *testing.T) {
	dir, _, order := mixedCodecChain(t)
	patterns := []string{`LINE [0-9]*3 `}
	pending := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns})
	if ref := pending.Answer.Chains["c1"]; ref.State != rxtypes.ChainStatePending {
		t.Fatalf("state %s", ref.State)
	}
	for _, m := range pending.Answer.Matches {
		if m.ChainLine != -1 || m.AbsoluteLineNumber < 1 || m.Chain == nil {
			t.Fatalf("pending match %+v", m)
		}
	}

	storeIndexes(t, dir, order[:len(order)-1]...)
	ready := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns})
	if ref := ready.Answer.Chains["c1"]; ref.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s after indexing", ref.State)
	}
	if diff := traceanswer.Difference(ready.Trace, pending.Trace, nil); diff != "" {
		t.Fatalf("the trace answers differ: %s", diff)
	}
	for i, m := range ready.Answer.Matches {
		named, _ := strconv.ParseInt(globalOf.FindStringSubmatch(*m.LineText)[1], 10, 64)
		if m.ChainLine != named || pending.Answer.Matches[i].ChainLine != -1 {
			t.Fatalf("match %d: chain_line %d (pending %d), the line names %d", i, m.ChainLine, pending.Answer.Matches[i].ChainLine, named)
		}
	}
}

// An invalid chain (two parts overlap) is still searched: its entry
// says invalid, with the reason, and no match has a chain line.
func TestSearch_AnInvalidChainIsSearchedWithoutChainLines(t *testing.T) {
	dir := overlapChain(t, 90*time.Second)
	answer := searchFor(t, SearchRequest{Paths: []string{filepath.Join(dir, "x.log")}, Patterns: []string{"LINE"}, Scan: true}).Answer
	ref := answer.Chains["c1"]
	if ref.State != rxtypes.ChainStateInvalid || len(ref.Reasons) != 1 || ref.Reasons[0].Code != rxtypes.ChainReasonOverlap || len(ref.Parts) != 2 {
		t.Fatalf("chain %+v", ref)
	}
	if len(answer.Matches) != 110 {
		t.Fatalf("%d matches, want every line of both parts", len(answer.Matches))
	}
	for _, m := range answer.Matches {
		if m.ChainLine != -1 || m.Chain == nil || *m.Chain != "c1" {
			t.Fatalf("match %+v", m)
		}
	}
}

// A part's own path is a file: searched alone, with no chain.
func TestSearch_APartsOwnPathIsAFile(t *testing.T) {
	dir, _, _ := mixedCodecChain(t)
	answer := searchFor(t, SearchRequest{Paths: []string{filepath.Join(dir, "app.log.2.gz")}, Patterns: []string{"LINE"}, Scan: true}).Answer
	if len(answer.Chains) != 0 || len(answer.Files) != 1 || len(answer.Matches) != 50 {
		t.Fatalf("chains %+v, files %v, %d matches", answer.Chains, answer.Files, len(answer.Matches))
	}
	for _, m := range answer.Matches {
		if m.Chain != nil || m.ChainLine != -1 {
			t.Fatalf("match %+v", m)
		}
	}
}

// A path that is neither a directory, nor a chain's handle, nor a file
// is refused with the cause; a handle whose active file does not exist
// names its chain all the same.
func TestSearch_APathThatNamesNothing(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "app.log-20261001.gz", text: timedLines(chainBase, time.Second, 1, 2, "1"), codec: compressedcopy.Gzip},
		{name: "app.log-20261002.gz", text: timedLines(chainBase.Add(time.Hour), time.Second, 3, 2, "2"), codec: compressedcopy.Gzip},
	})
	answer := searchFor(t, SearchRequest{Paths: []string{filepath.Join(dir, "app.log")}, Patterns: []string{"LINE"}, Scan: true}).Answer
	if len(answer.Chains) != 1 || len(answer.Matches) != 4 || answer.Matches[3].ChainLine != 4 {
		t.Fatalf("chains %+v, %d matches", answer.Chains, len(answer.Matches))
	}

	paths.Reset()
	_, err := Search(context.Background(), trace.New(), SearchRequest{Paths: []string{filepath.Join(dir, "nope.log")}, Patterns: []string{"LINE"}})
	var pathErr *SearchPathError
	if !errors.As(err, &pathErr) || !errors.Is(err, fs.ErrNotExist) || pathErr.Path != filepath.Join(dir, "nope.log") {
		t.Fatalf("error %v, want a SearchPathError for a missing path", err)
	}
}

// The trace cache works per part: a second search of a chain whose
// plain parts are large enough hits each part's entry and gives the
// same answer.
func TestSearch_TheSecondSearchHitsEachPartsTraceCache(t *testing.T) {
	t.Setenv("RX_LARGE_FILE_MB", "1")
	dir := t.TempDir()
	big := func(start time.Time, first int, part string) []byte {
		return timedLines(start, time.Millisecond, first, 30000, part)
	}
	older, newer := big(chainBase, 1, "1"), big(chainBase.Add(time.Hour), 30001, "active")
	if len(older) < 1<<20 {
		t.Fatalf("a part of %d bytes is below the cache's threshold", len(older))
	}
	writeChainFiles(t, dir, []chainFile{{name: "x.log.1", text: older}, {name: "x.log", text: newer}})
	patterns := []string{`LINE [0-9]*77 `}
	first := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns, Scan: true})
	for _, name := range []string{"x.log.1", "x.log"} {
		if _, err := trace.GetCachedScan(filepath.Join(dir, name), patterns, nil); err != nil {
			t.Fatalf("no trace cache entry for %s after the first search: %v", name, err)
		}
	}
	second := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: patterns, Scan: true})
	if diff := traceanswer.Difference(second.Trace, first.Trace, nil); diff != "" {
		t.Fatalf("the answers differ: %s", diff)
	}
	if !slices.Equal(foundLines(second.Answer), foundLines(first.Answer)) {
		t.Fatal("the chain lines differ")
	}
}

// A part the listing cannot read is not searched: it is skipped with
// its read error, has no file id, and its chain is invalid
// (unreadable), so no match has a chain line.
func TestSearch_APartThatCannotBeReadIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.2", text: timedLines(chainBase, time.Second, 1, 3, "2")},
		{name: "x.log.1", text: timedLines(chainBase.Add(time.Hour), time.Second, 4, 3, "1")},
		{name: "x.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 7, 3, "active")},
	})
	locked := filepath.Join(dir, "x.log.2")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	for _, p := range []string{dir, filepath.Join(dir, "x.log")} {
		answer := searchFor(t, SearchRequest{Paths: []string{p}, Patterns: []string{"LINE"}, Scan: true}).Answer
		if !slices.Equal(answer.SkipReasons, []rxtypes.SkippedFile{{Path: locked, Reason: paths.ReasonPermissionDenied}}) {
			t.Fatalf("%s: skips %+v", p, answer.SkipReasons)
		}
		ref := answer.Chains["c1"]
		if ref.State != rxtypes.ChainStateInvalid || len(ref.Parts) != 2 || ref.Reasons[0].Code != rxtypes.ChainReasonUnreadable {
			t.Fatalf("%s: chain %+v", p, ref)
		}
		// The chain's reason and the skip say the same fixed text.
		if want := "x.log.2 cannot be read: " + answer.SkipReasons[0].Reason; ref.Reasons[0].Message != want {
			t.Fatalf("%s: reason %q, want %q", p, ref.Reasons[0].Message, want)
		}
		if len(answer.Matches) != 6 {
			t.Fatalf("%s: %d matches, want the 6 lines of the readable parts", p, len(answer.Matches))
		}
		for _, m := range answer.Matches {
			if m.ChainLine != -1 || answer.Files[m.File] == locked {
				t.Fatalf("%s: match %+v in %s", p, m, answer.Files[m.File])
			}
		}
	}
}

// A chain of more than MaxParts parts is not read as one text: its
// files are searched as files of their own, where the walk (or, for a
// handle, the listing) lists them, and its entry has no parts and says
// too_many_parts. The test looks at what the search resolves to rather
// than run ripgrep over ten thousand files.
func TestSearch_AChainOfTooManyPartsIsSearchedAsFiles(t *testing.T) {
	dir := t.TempDir()
	for n := 2; n <= MaxParts+1; n++ {
		if err := os.WriteFile(filepath.Join(dir, "x.log."+strconv.Itoa(n)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, dir, map[string][]byte{"x.log": []byte("hit active\n"), "x.log.1": []byte("hit one\n"), "other.txt": []byte("hit\n")})

	for _, p := range []string{dir, filepath.Join(dir, "x.log")} {
		paths.Reset()
		r := newSearchResolver(SearchRequest{Paths: []string{p}, Scan: true})
		plan, err := r.resolve(context.Background())
		if err != nil {
			t.Fatalf("%s: resolve: %v", p, err)
		}
		wantFiles := MaxParts + 2 // x.log and x.log.1 to x.log.10001
		if p == dir {
			wantFiles++ // other.txt
		}
		if len(plan.Files) != wantFiles || len(r.partOf) != 0 {
			t.Fatalf("%s: %d files planned (%d parts), want %d files of their own", p, len(plan.Files), len(r.partOf), wantFiles)
		}
		if len(r.chains) != 1 {
			t.Fatalf("%s: %d chains", p, len(r.chains))
		}
		answer := r.answer(&rxtypes.TraceResponse{Matches: []rxtypes.Match{{File: "f1", AbsoluteLineNumber: 1}}})
		ref := answer.Chains["c1"]
		if ref.State != rxtypes.ChainStateInvalid || len(ref.Parts) != 0 || len(ref.Reasons) != 1 ||
			ref.Reasons[0].Code != rxtypes.ChainReasonTooManyParts {
			t.Fatalf("%s: chain %+v", p, ref)
		}
		if m := answer.Matches[0]; m.Chain != nil || m.ChainLine != -1 {
			t.Fatalf("%s: match %+v", p, m)
		}
	}
}

// Names that say the opposite of time (log4j2 numbers its newest file
// highest): a ready chain's parts are searched in time order, not in
// the order of their names, and the chain lines follow that order.
func TestSearch_PartsAreSearchedInTimeOrderWhenNamesDisagree(t *testing.T) {
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "app.log.1", text: timedLines(chainBase, time.Second, 1, 4, "1")},
		{name: "app.log.2", text: timedLines(chainBase.Add(time.Hour), time.Second, 5, 3, "2")},
		{name: "app.log", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 8, 2, "active")},
	})
	answer := searchFor(t, SearchRequest{Paths: []string{dir}, Patterns: []string{"LINE"}, Scan: true}).Answer
	wantOrder := []string{"app.log.1", "app.log.2", "app.log"}
	for i, name := range wantOrder {
		if id := "f" + strconv.Itoa(i+1); answer.Files[id] != filepath.Join(dir, name) {
			t.Fatalf("files[%s] = %s, want %s (time order)", id, answer.Files[id], name)
		}
	}
	for i, m := range answer.Matches {
		if m.ChainLine != int64(i+1) {
			t.Fatalf("match %d (%s): chain_line %d, want %d", i, *m.LineText, m.ChainLine, i+1)
		}
	}
}

// A directory given as a relative path keeps its spelling: the parts'
// paths in files and the duplicate's path in skipped_files are spelled
// as the walk reaches them, and the chain's parts name those files.
func TestSearch_ARelativeDirectoryKeepsItsSpelling(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := timedLines(chainBase, time.Second, 1, 2, "1")
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.1", text: first},
		{name: "x.log.1.gz", text: first, codec: compressedcopy.Gzip},
		{name: "x.log", text: timedLines(chainBase.Add(time.Hour), time.Second, 3, 2, "active")},
	})
	t.Chdir(parent)
	answer := searchFor(t, SearchRequest{Paths: []string{"./logs"}, Patterns: []string{"LINE"}, Scan: true}).Answer
	ref := answer.Chains["c1"]
	if got := []string{answer.Files[ref.Parts[0]], answer.Files[ref.Parts[1]]}; !slices.Equal(got, []string{"./logs/x.log.1", "./logs/x.log"}) {
		t.Fatalf("parts %v are %v", ref.Parts, got)
	}
	if !slices.Equal(answer.SkippedFiles, []string{"./logs/x.log.1.gz"}) {
		t.Fatalf("skipped %v", answer.SkippedFiles)
	}
	if ref.Path != filepath.Join("logs", "x.log") || ref.State != rxtypes.ChainStateReady || len(answer.Matches) != 4 {
		t.Fatalf("chain %+v, %d matches", ref, len(answer.Matches))
	}
}

// A file or a handle classifies only the files that can belong to the
// chain it names, never the rest of its directory: a file beside
// thousands of another chain's parts opens itself alone, and a handle
// opens its own chain's files.
func TestSearch_AFileOrAHandleClassifiesOnlyItsOwnChain(t *testing.T) {
	dir := t.TempDir()
	for n := 1; n <= 3000; n++ {
		if err := os.WriteFile(filepath.Join(dir, "big.log."+strconv.Itoa(n)), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, dir, map[string][]byte{"plain.txt": []byte("hit\n"), "app.log": []byte("a\n"), "app.log.1": []byte("b\n")})
	for _, tc := range []struct {
		name       string
		classified []string
	}{
		{"plain.txt", []string{"plain.txt"}},
		{"app.log", []string{"app.log", "app.log.1"}},
	} {
		paths.Reset()
		r := newSearchResolver(SearchRequest{Paths: []string{filepath.Join(dir, tc.name)}})
		plan, err := r.resolve(context.Background())
		if err != nil {
			t.Fatalf("%s: resolve: %v", tc.name, err)
		}
		var classified []string
		for path := range r.files {
			classified = append(classified, filepath.Base(path))
		}
		slices.Sort(classified)
		if !slices.Equal(classified, tc.classified) || len(plan.Files) != len(tc.classified) {
			t.Fatalf("%s: %d files planned, %d classified, want %v", tc.name, len(plan.Files), len(classified), tc.classified)
		}
	}
}
