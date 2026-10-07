package logchain

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"sync"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
)

// maxCachedDescriptions is how many chains' frozen-part data the
// description cache holds; past it, the one used least recently is
// dropped.
const maxCachedDescriptions = 64

// maxCachedParts is how many parts' facts the description cache holds
// over all its entries: four chains of the largest size (MaxParts), or
// all 64 entries of usual ones. Past it, the entries used least
// recently are dropped.
//
// SECURITY: an entry holds a few hundred bytes per part, so the bound
// keeps the cache to some tens of megabytes however large the chains
// that clients describe.
const maxCachedParts = 4 * MaxParts

// descriptions is the process's description cache. `rx serve` keeps it
// for its lifetime; a CLI run starts empty.
var descriptions = newDescriptionCache(maxCachedDescriptions, maxCachedParts)

// descriptionCache keeps, for chains whose frozen parts all have a
// current line index, what describing read about those parts (their
// partFacts), so the next describe of the same files reads no index of
// a frozen part.
//
// What is kept is only what the frozen parts' files and indexes decide,
// and the key holds everything else it depends on (cacheKey): the
// handle, a digest of every part's stat (which changes when any frozen
// part's name, inode, size, modification time or ctime does), and the
// zones the times are read in. What the key cannot hold, the index
// files the facts came from, a hit checks before it is used (see
// indexStamp): an index removed, rebuilt or no longer describing its
// part drops the entry. A hit therefore gives the facts a miss would
// read again. The active part's facts, the reasons, the order and the
// state are computed on every describe, because the active part grows
// without changing the key and the overlap tolerance is read each time.
//
// It holds at most limit entries and partLimit part facts in all.
//
// It is safe for the concurrent requests of `rx serve`: one mutex
// guards the map, the recency list and the part count, and no file is
// read while it is held.
type descriptionCache struct {
	mu    sync.Mutex
	limit int
	// partLimit is the most part facts the entries hold together, and
	// parts how many they hold now.
	partLimit, parts int
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

// newDescriptionCache returns an empty cache of at most limit entries
// and partLimit part facts in all.
func newDescriptionCache(limit, partLimit int) *descriptionCache {
	return &descriptionCache{limit: limit, partLimit: partLimit, recent: list.New(), entries: map[string]*list.Element{}}
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

// put keeps a copy of facts under key, in place of any entry the key
// had, then drops the entries used least recently until the cache holds
// at most its limits. Facts of more parts than partLimit are not kept:
// they would push out every other entry and still not fit.
func (c *descriptionCache) put(key string, facts []partFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(facts) > c.partLimit {
		return
	}
	if element, ok := c.entries[key]; ok {
		c.remove(element)
	}
	c.entries[key] = c.recent.PushFront(&cachedFacts{key: key, facts: slices.Clone(facts)})
	c.parts += len(facts)
	// INVARIANT: the new entry is at the front and alone within both
	// limits, so the loop stops before it reaches it.
	for c.recent.Len() > c.limit || c.parts > c.partLimit {
		c.remove(c.recent.Back())
	}
}

// drop removes the entry kept under key, if there is one.
func (c *descriptionCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.remove(element)
	}
}

// remove takes one entry out of the list, the map and the part count.
// The caller holds the mutex.
func (c *descriptionCache) remove(element *list.Element) {
	entry := element.Value.(*cachedFacts)
	c.recent.Remove(element)
	delete(c.entries, entry.key)
	c.parts -= len(entry.facts)
}

// heldParts is how many part facts the cache holds in all.
func (c *descriptionCache) heldParts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parts
}

// size is how many entries the cache holds.
func (c *descriptionCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recent.Len()
}

// cacheKey is the key of a chain's facts: its handle, partsDigest of
// its parts, the file zone and RX_LOG_TZ (the zone a part whose
// timestamps carry none is read in without a file zone). The pieces
// are separated by a NUL byte, which no path or zone name holds.
func cacheKey(c Candidate, fileZone config.Zone) string {
	return strings.Join([]string{c.Handle(), partsDigest(c), fileZone.Name, config.LogTZ().Name}, "\x00")
}

// partsDigest is a SHA-256, all 64 hex digits of it, over what
// Fingerprint hashes of each part (its name, device and inode, and a
// frozen part's size and modification time) and each frozen part's
// ctime, from the stat the listing took.
//
// SECURITY: the fingerprint keeps 64 bits of a hash over fields a user
// who can write a frozen part sets at will (its size and its mtime, to
// the nanosecond), so a collision could be searched for, and the facts
// read from one set of files served for another. The key keeps every
// bit, and hashes the ctime as well, which the kernel sets on every
// change and no user can choose.
func partsDigest(c Candidate) string {
	h := sha256.New()
	for _, p := range c.Parts {
		writeFingerprintPart(h, p)
		if p.IsActive {
			continue
		}
		var ctime int64
		if changed := index.StatIdentity(p.Info).ChangedNs; changed != nil {
			ctime = *changed
		}
		// Go note: binary.Write to a hash.Hash never fails (see
		// writeFingerprintPart).
		_ = binary.Write(h, binary.LittleEndian, ctime)
	}
	return hex.EncodeToString(h.Sum(nil))
}
