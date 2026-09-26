package logchain

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/paths"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// readCounts records what describing read, by part name: each seam
// describing reads a part through.
type readCounts struct {
	mu         sync.Mutex
	indexLoads map[string]int
	opens      map[string]int
	builds     map[string]int
	timeRanges map[string]int
}

// countReads makes every seam count its calls for the rest of the test.
func countReads(t *testing.T) *readCounts {
	t.Helper()
	counts := &readCounts{
		indexLoads: map[string]int{}, opens: map[string]int{}, builds: map[string]int{}, timeRanges: map[string]int{},
	}
	record := func(m map[string]int, path string) {
		counts.mu.Lock()
		defer counts.mu.Unlock()
		m[filepath.Base(path)]++
	}
	load, open, build, timeRange := loadPartIndex, openPart, buildPartIndex, readTimeRange
	t.Cleanup(func() { loadPartIndex, openPart, buildPartIndex, readTimeRange = load, open, build, timeRange })
	loadPartIndex = func(p paths.Pinned) (*rxtypes.UnifiedFileIndex, error) {
		record(counts.indexLoads, p.Path())
		return load(p)
	}
	openPart = func(p paths.Pinned) (*os.File, error) {
		record(counts.opens, p.Path())
		return open(p)
	}
	buildPartIndex = func(path string) (*rxtypes.UnifiedFileIndex, error) {
		record(counts.builds, path)
		return build(path)
	}
	readTimeRange = func(ctx context.Context, req samples.Request) (*rxtypes.TimeRangeResponse, error) {
		record(counts.timeRanges, req.Path)
		return timeRange(ctx, req)
	}
	return counts
}

// total is how many reads of any kind were counted.
func (c *readCounts) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range []map[string]int{c.indexLoads, c.opens, c.builds, c.timeRanges} {
		for _, v := range m {
			n += v
		}
	}
	return n
}

// reset forgets what was counted so far.
func (c *readCounts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range []map[string]int{c.indexLoads, c.opens, c.builds, c.timeRanges} {
		clear(m)
	}
}

// indexedChain is a ready chain of three frozen parts, each with a
// stored index, and an active file without one.
func indexedChain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeChainFiles(t, dir, []chainFile{
		{name: "x.log.3", text: timedLines(chainBase, time.Second, 1, 50, "3")},
		{name: "x.log.2", text: timedLines(chainBase.Add(time.Hour), time.Second, 51, 50, "2")},
		{name: "x.log.1", text: timedLines(chainBase.Add(2*time.Hour), time.Second, 101, 50, "1")},
		{name: "x.log", text: timedLines(chainBase.Add(3*time.Hour), time.Second, 151, 50, "active")},
	})
	storeIndexes(t, dir, "x.log.3", "x.log.2", "x.log.1")
	return dir
}

// Describing a ready chain reads each frozen part's index and the
// active file's head and tail, and nothing else: no frozen part is
// opened and no index is built. (samples.TimeRange holds the read of the
// active file to its head and at most 16 MiB from its end; its own
// budget tests count those bytes.)
func TestDescribe_ReadsTheIndexesAndTheActiveFileOnly(t *testing.T) {
	dir := indexedChain(t)
	c := resolveIn(t, dir, "x.log")
	counts := countReads(t)
	d, err := Describe(context.Background(), c, Options{})
	if err != nil || d.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("describe: %v %+v", err, d)
	}
	wantLoads := map[string]int{"x.log.3": 1, "x.log.2": 1, "x.log.1": 1, "x.log": 1}
	if !maps.Equal(counts.indexLoads, wantLoads) || len(counts.opens) != 0 || len(counts.builds) != 0 ||
		!maps.Equal(counts.timeRanges, map[string]int{"x.log": 1}) {
		t.Fatalf("reads: index %v, opens %v, builds %v, time ranges %v",
			counts.indexLoads, counts.opens, counts.builds, counts.timeRanges)
	}
}

// A second describe of a ready chain reads no frozen part's index: the
// memory cache holds what they said. The active file is read again, and
// the answer is the same.
func TestDescribe_TheCacheSkipsTheFrozenParts(t *testing.T) {
	dir := indexedChain(t)
	counts := countReads(t)
	first := describe(t, dir, "x.log", Options{})
	counts.reset()
	second := describe(t, dir, "x.log", Options{})
	if !maps.Equal(counts.indexLoads, map[string]int{"x.log": 1}) || !maps.Equal(counts.timeRanges, map[string]int{"x.log": 1}) {
		t.Fatalf("second describe read index %v, time ranges %v", counts.indexLoads, counts.timeRanges)
	}
	if jsonOf(t, first.Response) != jsonOf(t, second.Response) {
		t.Fatalf("a cache hit answers otherwise:\n%s\n%s", jsonOf(t, first.Response), jsonOf(t, second.Response))
	}
	// Another file zone is another entry.
	counts.reset()
	zone, err := config.ParseZone("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	describe(t, dir, "x.log", Options{FileZone: zone})
	if counts.indexLoads["x.log.1"] != 1 {
		t.Fatalf("a describe in another zone used the cache: %v", counts.indexLoads)
	}
}

// A chain with a frozen part that has no index is not kept: the next
// describe reads every part again.
func TestDescribe_APendingChainIsNotCached(t *testing.T) {
	dir := indexedChain(t)
	if err := os.WriteFile(filepath.Join(dir, "x.log.4"), timedLines(chainBase.Add(-time.Hour), time.Second, 1, 5, "4"), 0o600); err != nil {
		t.Fatal(err)
	}
	counts := countReads(t)
	for range 2 {
		counts.reset()
		d := describe(t, dir, "x.log", Options{})
		if d.Response.State != rxtypes.ChainStatePending {
			t.Fatalf("state %s", d.Response.State)
		}
		if counts.indexLoads["x.log.1"] != 1 || counts.opens["x.log.4"] != 1 {
			t.Fatalf("index loads %v, opens %v", counts.indexLoads, counts.opens)
		}
	}
	// Scanned parts are read, not indexed: such a chain is not kept
	// either.
	for range 2 {
		counts.reset()
		describe(t, dir, "x.log", Options{Scan: true})
		if counts.builds["x.log.4"] != 1 {
			t.Fatalf("builds %v", counts.builds)
		}
	}
}

// The cache holds at most its limit, dropping the entry used least
// recently.
func TestDescriptionCache_HoldsAtMostItsLimit(t *testing.T) {
	cache := newDescriptionCache(maxCachedDescriptions, maxCachedParts)
	facts := []partFacts{{indexed: true}}
	for i := range maxCachedDescriptions {
		cache.put(strconv.Itoa(i), facts)
	}
	// Using entry 0 keeps it; entry 1 is now the oldest.
	if _, ok := cache.get("0", 1); !ok {
		t.Fatal("entry 0 missing")
	}
	cache.put("new", facts)
	if cache.size() != maxCachedDescriptions {
		t.Fatalf("%d entries", cache.size())
	}
	if _, ok := cache.get("1", 1); ok {
		t.Fatal("the least recently used entry was kept")
	}
	for _, key := range []string{"0", "2", "new"} {
		if _, ok := cache.get(key, 1); !ok {
			t.Fatalf("entry %s dropped", key)
		}
	}
	// A hit is a copy: changing it changes no later hit.
	got, _ := cache.get("new", 1)
	got[0].indexed = false
	if again, _ := cache.get("new", 1); !again[0].indexed {
		t.Fatal("a hit shares its slice with the cache")
	}
}

// The cache holds at most its limit of part facts in all, dropping the
// entries used least recently; an entry larger than the limit alone is
// not kept.
func TestDescriptionCache_HoldsAtMostItsPartLimit(t *testing.T) {
	cache := newDescriptionCache(maxCachedDescriptions, 10)
	cache.put("a", make([]partFacts, 4))
	cache.put("b", make([]partFacts, 4))
	cache.put("c", make([]partFacts, 4))
	if _, ok := cache.get("a", 4); ok {
		t.Fatal("the cache holds more parts than its limit")
	}
	cache.put("huge", make([]partFacts, 11))
	if _, ok := cache.get("huge", 11); ok {
		t.Fatal("an entry larger than the limit was kept")
	}
	for _, key := range []string{"b", "c"} {
		if _, ok := cache.get(key, 4); !ok {
			t.Fatalf("entry %s dropped", key)
		}
	}
	if cache.size() != 2 || cache.heldParts() != 8 {
		t.Fatalf("%d entries of %d parts", cache.size(), cache.heldParts())
	}
	// Putting a key again replaces its entry, and its parts are counted
	// once.
	cache.put("b", make([]partFacts, 2))
	if cache.size() != 2 || cache.heldParts() != 6 {
		t.Fatalf("after a replacement: %d entries of %d parts", cache.size(), cache.heldParts())
	}
}

// describeUncached describes the chain named name in dir with an empty
// cache, and leaves the shared cache as it was: what a cache miss
// answers.
func describeUncached(t *testing.T, dir, name string, opts Options) *Description {
	t.Helper()
	shared := descriptions
	descriptions = newDescriptionCache(maxCachedDescriptions, maxCachedParts)
	defer func() { descriptions = shared }()
	return describe(t, dir, name, opts)
}

// A cache hit gives what a miss gives when a frozen part's index went
// away, came back, or no longer describes its part: the hit looks at
// each index file (a stat, no read) and drops the entry when one
// changed.
func TestDescribe_ACacheHitNoticesAChangedIndex(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	dir := indexedChain(t)
	counts := countReads(t)
	describe(t, dir, "x.log", Options{})
	cachedIndex := index.GetCachePath(filepath.Join(dir, "x.log.2"))

	steps := []struct {
		label  string
		change func()
		state  string
	}{
		{"the index of a frozen part is removed", func() {
			if err := os.Remove(cachedIndex); err != nil {
				t.Fatal(err)
			}
		}, rxtypes.ChainStatePending},
		{"the index is built again", func() { storeIndexes(t, dir, "x.log.2") }, rxtypes.ChainStateReady},
		{"the index is rebuilt in place", func() { storeIndexes(t, dir, "x.log.2") }, rxtypes.ChainStateReady},
		{"the part's ctime moves (chmod), so its index is stale", func() {
			if err := os.Chmod(filepath.Join(dir, "x.log.1"), 0o640); err != nil {
				t.Fatal(err)
			}
		}, rxtypes.ChainStatePending},
	}
	for _, step := range steps {
		// A describe with nothing changed fills the cache, so the next
		// one is a hit unless the change below is noticed.
		describe(t, dir, "x.log", Options{})
		step.change()
		counts.reset()
		hit := describe(t, dir, "x.log", Options{})
		miss := describeUncached(t, dir, "x.log", Options{})
		if hit.Response.State != step.state || jsonOf(t, hit.Response) != jsonOf(t, miss.Response) {
			t.Fatalf("%s: state %s, want %s; the cached answer and a miss differ:\n%s\n%s",
				step.label, hit.Response.State, step.state, jsonOf(t, hit.Response), jsonOf(t, miss.Response))
		}
		if counts.indexLoads["x.log.3"] == 0 {
			t.Fatalf("%s: the cache entry was used: index loads %v", step.label, counts.indexLoads)
		}
	}
}

// A ready chain with an empty frozen part is cached: an empty part has
// no index, and its facts follow from its size, which the key holds.
func TestDescribe_AChainWithAnEmptyFrozenPartIsCached(t *testing.T) {
	dir := indexedChain(t)
	if err := os.WriteFile(filepath.Join(dir, "x.log.4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	counts := countReads(t)
	first := describe(t, dir, "x.log", Options{})
	if first.Response.State != rxtypes.ChainStateReady {
		t.Fatalf("state %s", first.Response.State)
	}
	counts.reset()
	second := describe(t, dir, "x.log", Options{})
	if !maps.Equal(counts.indexLoads, map[string]int{"x.log": 1}) {
		t.Fatalf("second describe read index %v", counts.indexLoads)
	}
	if jsonOf(t, first.Response) != jsonOf(t, second.Response) {
		t.Fatalf("a cache hit answers otherwise:\n%s\n%s", jsonOf(t, first.Response), jsonOf(t, second.Response))
	}
}

// The cache key holds every frozen part's ctime, which no user can set,
// beside the stat fields the fingerprint covers: two listings whose
// parts differ only in their ctime share a fingerprint and never a key.
func TestCacheKey_HoldsEveryFrozenPartsChangeTime(t *testing.T) {
	dir := indexedChain(t)
	before := resolveIn(t, dir, "x.log")
	if err := os.Chmod(filepath.Join(dir, "x.log.2"), 0o640); err != nil {
		t.Fatal(err)
	}
	after := resolveIn(t, dir, "x.log")
	if Fingerprint(before) != Fingerprint(after) {
		t.Fatal("a chmod changed the fingerprint; the test proves nothing")
	}
	var zone config.Zone
	if _, _, ok := index.InodeAndDevice(after.Parts[1].Info); ok && cacheKey(before, zone) == cacheKey(after, zone) {
		t.Fatal("two listings with different ctimes share a cache key")
	}
}
