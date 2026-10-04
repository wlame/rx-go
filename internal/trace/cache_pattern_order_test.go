package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/testutil/seekablefile"
	"github.com/wlame/rx-go/internal/testutil/traceanswer"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// The trace cache keys an entry on the sorted patterns, so searches that
// list the same patterns in any order share one entry. Each record in the
// entry names its pattern by its position in the order of the search
// that wrote it. A search that reads the entry with its patterns in
// another order is answered exactly as its own scan answers it: every
// match carries the reader's ID for the pattern it matched, and that
// pattern's submatches.

// patternOrderText is a log of about size bytes whose lines carry tags
// that the patterns of these tests tell apart: some lines match exactly
// one pattern, some match two or three, and some match none. Each line
// also holds random hex, so a seekable-zstd copy stays over the 1 MB a
// compressed file needs before its scan is cached.
func patternOrderText(size int) []byte {
	// tagsEvery: a line whose number is a multiple of the key gets the tag.
	tagsEvery := []struct {
		every int
		tag   string
	}{
		{30, "WARN"},
		{50, "NEEDLE"},
		{70, "Needle"},
		{110, "a.b"},
		{130, "axb"},
	}
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // reproducible fixture bytes, not a secret
	hex := make([]byte, 24)
	var b bytes.Buffer
	for line := 1; b.Len() < size; line++ {
		_, _ = rng.Read(hex)
		fmt.Fprintf(&b, "LINE %d %x", line, hex)
		for _, t := range tagsEvery {
			if line%t.every == 0 {
				b.WriteString(" " + t.tag)
			}
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// patternOrderSize is the size of the fixture: several chunks of a
// plain file under largeFileCacheEnv, and over 1 MB compressed.
const patternOrderSize = 3 << 20

// writePatternOrderLog writes patternOrderText as a plain file.
func writePatternOrderLog(t *testing.T) string {
	t.Helper()
	return writeTextFile(t, "order.log", patternOrderText(patternOrderSize))
}

// cacheEntryFiles returns the content of every trace cache entry, keyed
// by path. A scan rewrites its entry with a new created_at and a cache
// hit leaves it alone, so an unchanged snapshot shows a hit on any kind
// of file.
func cacheEntryFiles(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(config.GetTraceCacheDir(), "*", "*.json"))
	if err != nil {
		t.Fatalf("list cache entries: %v", err)
	}
	contents := make(map[string]string, len(files))
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // path found under the test's cache dir
		if err != nil {
			t.Fatalf("read cache entry: %v", err)
		}
		contents[f] = string(body)
	}
	return contents
}

// traceThroughCache runs a trace that must be answered from the cache,
// and fails the test when it scanned the file instead.
func traceThroughCache(t *testing.T, path string, patterns []string, opts Options) *rxtypes.TraceResponse {
	t.Helper()
	before := cacheEntryFiles(t)
	if len(before) == 0 {
		t.Fatal("no trace cache entry to read")
	}
	resp := traceOnce(t, path, patterns, opts)
	if after := cacheEntryFiles(t); !maps.Equal(after, before) {
		t.Fatalf("the trace of %q scanned the file instead of reading the cache", patterns)
	}
	return resp
}

// requireEveryPatternMatches fails t unless the answer credits at least
// one match to each of its pattern IDs, so a test of pattern labels
// cannot pass on a fixture where a pattern has nothing to label.
func requireEveryPatternMatches(t *testing.T, resp *rxtypes.TraceResponse) {
	t.Helper()
	for pid, pattern := range resp.Patterns {
		if !slices.ContainsFunc(resp.Matches, func(m rxtypes.Match) bool { return m.Pattern == pid }) {
			t.Fatalf("the fixture has no match for %s %q", pid, pattern)
		}
	}
}

// permutations returns every order of xs.
func permutations(xs []string) [][]string {
	if len(xs) <= 1 {
		return [][]string{slices.Clone(xs)}
	}
	var out [][]string
	for i := range xs {
		rest := append(slices.Clone(xs[:i]), xs[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{xs[i]}, p...))
		}
	}
	return out
}

// A cache entry written with the patterns in one order answers a search
// with them in any other order exactly as that search's own scan does,
// in every field: each match's pattern ID and submatches included.
func TestCacheHitAnswersAsTheScanWhateverOrderThePatternsComeIn(t *testing.T) {
	cases := []struct {
		name    string
		written []string
		reads   [][]string
		flags   []string
	}{
		{name: "two patterns", written: []string{"WARN", "NEEDLE"},
			reads: [][]string{{"NEEDLE", "WARN"}, {"WARN", "NEEDLE"}}},
		{name: "three patterns", written: []string{"WARN", "NEEDLE", "a.b"},
			reads: permutations([]string{"WARN", "NEEDLE", "a.b"})},
		{name: "a pattern given twice", written: []string{"WARN", "WARN", "NEEDLE"},
			reads: [][]string{{"WARN", "NEEDLE", "WARN"}, {"NEEDLE", "WARN", "WARN"}, {"WARN", "WARN", "NEEDLE"}}},
		{name: "ignore case", written: []string{"needle", "WARN"}, flags: []string{"-i"},
			reads: [][]string{{"WARN", "needle"}}},
		{name: "fixed strings", written: []string{"a.b", "WARN"}, flags: []string{"-F"},
			reads: [][]string{{"WARN", "a.b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			largeFileCacheEnv(t)
			path := writePatternOrderLog(t)
			opts := Options{RgExtraArgs: tc.flags}
			written := traceOnce(t, path, tc.written, opts)
			if written.FileChunks["f1"] < 2 {
				t.Fatalf("fixture scanned in %d chunks; the test needs several", written.FileChunks["f1"])
			}

			for _, read := range tc.reads {
				hit := traceThroughCache(t, path, read, opts)
				noCache := opts
				noCache.NoCache = true
				scan := traceOnce(t, path, read, noCache)
				requireEveryPatternMatches(t, scan)
				traceanswer.RequireSame(t, fmt.Sprintf("cache hit for %q written as %q", read, tc.written), hit, scan)
			}
		})
	}
}

// A capped trace answered from an entry written in another order keeps
// the first max_results matches of the scan's answer, each labeled
// with the pattern it matched.
func TestCappedCacheHitLabelsEachMatchWithItsOwnPattern(t *testing.T) {
	largeFileCacheEnv(t)
	path := writePatternOrderLog(t)
	traceOnce(t, path, []string{"WARN", "NEEDLE"}, Options{})

	read := []string{"NEEDLE", "WARN"}
	limit := 25
	hit := traceThroughCache(t, path, read, Options{MaxResults: &limit})
	scan := traceOnce(t, path, read, Options{NoCache: true})

	if len(hit.Matches) != limit {
		t.Fatalf("capped hit returned %d matches, want %d", len(hit.Matches), limit)
	}
	traceanswer.RequireSame(t, "capped cache hit against the scan cut to the cap", hit.Matches, scan.Matches[:limit])
}

// A seekable-zstd file's entry, read through the frames it names, labels
// each match as the scan of the reader's order does.
func TestSeekableCacheHitAnswersAsTheScanWhateverOrderThePatternsComeIn(t *testing.T) {
	largeFileCacheEnv(t)
	text := patternOrderText(patternOrderSize)
	path := filepath.Join(t.TempDir(), "order.log.zst")
	seekablefile.Write(t, path, seekablefile.SplitEvery(text, 256<<10))
	if info, err := os.Stat(path); err != nil || info.Size() < 1<<20 {
		t.Fatalf("the seekable fixture must be at least 1 MB to be cached (stat: %v, %v)", info, err)
	}

	written := []string{"WARN", "NEEDLE", "a.b"}
	traceOnce(t, path, written, Options{})
	for _, read := range permutations(written) {
		hit := traceThroughCache(t, path, read, Options{})
		scan := traceOnce(t, path, read, Options{NoCache: true})
		requireEveryPatternMatches(t, scan)
		traceanswer.RequireSame(t, fmt.Sprintf("seekable cache hit for %q", read), hit, scan)
	}
}

// rewriteCacheEntry loads the cache entry at cachePath, lets change edit
// it and writes it back.
func rewriteCacheEntry(t *testing.T, cachePath string, change func(*rxtypes.TraceCacheData)) {
	t.Helper()
	raw, err := os.ReadFile(cachePath) //nolint:gosec // path built by CachePath
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var data rxtypes.TraceCacheData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse cache: %v", err)
	}
	change(&data)
	patched, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, patched, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

// An entry that cannot say which of the reader's patterns a record
// belongs to is a miss: the trace scans and answers as the scan does,
// never with a guessed label.
func TestCacheEntryThatDoesNotNameTheReadersPatternsIsAMiss(t *testing.T) {
	patterns := []string{"WARN", "NEEDLE"}
	cases := []struct {
		name   string
		change func(*rxtypes.TraceCacheData)
	}{
		{"a stored pattern the reader does not have", func(d *rxtypes.TraceCacheData) {
			d.Patterns = []string{"WARN", "SOMETHING ELSE"}
		}},
		{"fewer stored patterns than the reader has", func(d *rxtypes.TraceCacheData) {
			d.Patterns = []string{"WARN"}
		}},
		{"one stored pattern twice", func(d *rxtypes.TraceCacheData) {
			d.Patterns = []string{"WARN", "WARN"}
		}},
		{"a record past the stored patterns", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].PatternIndex = len(d.Patterns)
		}},
		{"a negative pattern index", func(d *rxtypes.TraceCacheData) {
			d.Matches[0].PatternIndex = -1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			largeFileCacheEnv(t)
			path := writePatternOrderLog(t)
			traceOnce(t, path, patterns, Options{})
			rewriteCacheEntry(t, CachePath(path, patterns, nil), tc.change)

			read := []string{"NEEDLE", "WARN"}
			if _, err := GetCachedScan(path, read, nil); err == nil {
				t.Fatal("the entry was accepted")
			}
			got := traceOnce(t, path, read, Options{})
			scan := traceOnce(t, path, read, Options{NoCache: true})
			traceanswer.RequireSame(t, "trace over an inconsistent entry", got, scan)
		})
	}
}
