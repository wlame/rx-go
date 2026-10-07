package logchain

import (
	"container/list"
	"slices"
	"strings"
	"sync"

	"github.com/wlame/rx-go/internal/config"
)

// maxCachedDescriptions is how many chains' frozen-part data the
// description cache holds; past it, the one used least recently is
// dropped.
const maxCachedDescriptions = 64

// descriptions is the process's description cache. `rx serve` keeps it
// for its lifetime; a CLI run starts empty.
var descriptions = newDescriptionCache(maxCachedDescriptions)

// descriptionCache keeps, for chains whose frozen parts all have a
// current line index, what describing read about those parts (their
// partFacts), so the next describe of the same files reads no index of
// a frozen part.
//
// What is kept is only what the frozen parts' files and indexes decide,
// and the key holds everything else it depends on (cacheKey): the
// handle, the fingerprint (which changes when any frozen part's name,
// inode, size or modification time does), and the zones the times are
// read in. A hit therefore gives the facts a miss would read again. The
// active part's facts, the reasons, the order and the state are
// computed on every describe, because the active part grows without
// changing the fingerprint and the overlap tolerance is read each time.
//
// It is safe for the concurrent requests of `rx serve`: one mutex
// guards the map and the recency list, and no file is read while it is
// held.
type descriptionCache struct {
	mu    sync.Mutex
	limit int
	// recent holds the keys, most recently used at the front.
	// Go note: container/list is a doubly linked list; moving or
	// removing an element is O(1), which a least-recently-used cache
	// needs on every hit.
	recent *list.List
	// entries maps a key to its element in recent, whose Value is a
	// *cachedFacts.
	entries map[string]*list.Element
}

// cachedFacts is one entry: the key, kept to remove the map entry when
// the element is dropped, and the facts of every part of the chain (the
// active part's entry is not used).
type cachedFacts struct {
	key   string
	facts []partFacts
}

// newDescriptionCache returns an empty cache of at most limit entries.
func newDescriptionCache(limit int) *descriptionCache {
	return &descriptionCache{limit: limit, recent: list.New(), entries: map[string]*list.Element{}}
}

// get returns a copy of the facts kept under key, for a chain of parts
// parts, and whether there were any.
func (c *descriptionCache) get(key string, parts int) ([]partFacts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*cachedFacts)
	if len(entry.facts) != parts {
		return nil, false
	}
	c.recent.MoveToFront(element)
	// The caller writes the active part's facts into its slice, so it
	// gets a copy of its own.
	return slices.Clone(entry.facts), true
}

// put keeps a copy of facts under key, dropping the least recently used
// entry when the cache is full.
func (c *descriptionCache) put(key string, facts []partFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value.(*cachedFacts).facts = slices.Clone(facts)
		c.recent.MoveToFront(element)
		return
	}
	c.entries[key] = c.recent.PushFront(&cachedFacts{key: key, facts: slices.Clone(facts)})
	for c.recent.Len() > c.limit {
		oldest := c.recent.Back()
		c.recent.Remove(oldest)
		delete(c.entries, oldest.Value.(*cachedFacts).key)
	}
}

// size is how many entries the cache holds.
func (c *descriptionCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recent.Len()
}

// cacheKey is the key of a chain's facts: its handle, its fingerprint,
// the file zone and RX_LOG_TZ (the zone a part whose timestamps carry
// none is read in without a file zone). The parts are separated by a
// NUL byte, which no path or zone name holds.
func cacheKey(c Candidate, fingerprint string, fileZone config.Zone) string {
	return strings.Join([]string{c.Handle(), fingerprint, fileZone.Name, config.LogTZ().Name}, "\x00")
}
