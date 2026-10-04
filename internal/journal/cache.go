package journal

// This file holds the DATA cache: what reading each DATA object gave,
// for the objects read last, by object offset. Entries share DATA
// objects (one hostname, one unit, one identifier serve thousands of
// entries), so an object read once serves every later entry that lists
// it: the decoded payload of a field the renderer reads, or only the fact
// that the object holds another field, whose value is never decoded.

// defaultCacheBytes is the cache's bound when Options leaves it 0.
const defaultCacheBytes = 32 << 20

// cacheEntryOverhead is what the cache counts, on top of a payload's
// bytes, for holding it: the map entry and the list node. Counting it
// keeps a cache of many tiny payloads, or of many objects of fields the
// renderer does not read, within its bound too.
const cacheEntryOverhead = 96

// cacheNode is one object the cache holds, in a list from the most
// recently used (head) to the least (tail): the payload of a field the
// renderer reads, or unread set for any other field.
type cacheNode struct {
	offset     uint64
	payload    []byte
	unread     bool
	prev, next *cacheNode
}

// dataCache holds what reading DATA objects gave, by object offset,
// least recently used out first, while the bytes it holds (each payload
// plus cacheEntryOverhead) stay within limit. A payload that alone
// exceeds the limit is not held. The cache never changes a payload it
// hands out: the slices are shared with the entries that use them, which
// only read them.
//
// It is used by one goroutine (the reader's), so it takes no lock.
type dataCache struct {
	limit    int64
	byOffset map[uint64]*cacheNode
	// head is the most recently used node, tail the least.
	head, tail *cacheNode

	// held is the bytes held now, maxHeld the most ever held; hits and
	// misses count get's answers. Tests read them.
	held, maxHeld int64
	hits, misses  int64
}

// newDataCache returns an empty cache bounded by limit bytes; a limit of
// 0 or less holds nothing.
func newDataCache(limit int64) *dataCache {
	return &dataCache{limit: limit, byOffset: map[uint64]*cacheNode{}}
}

// get returns what the cache holds for the DATA object at offset, if
// anything (cached): its payload, or unread for an object of a field the
// renderer does not read. It marks the object the most recently used.
func (c *dataCache) get(offset uint64) (payload []byte, unread, cached bool) {
	node, ok := c.byOffset[offset]
	if !ok {
		c.misses++
		return nil, false, false
	}
	c.hits++
	c.unlink(node)
	c.pushFront(node)
	return node.payload, node.unread, true
}

// put holds payload as the DATA object at offset's, the most recently
// used, evicting the least recently used objects until it fits. An
// object already held, or a payload larger than the limit, is left as it
// is.
func (c *dataCache) put(offset uint64, payload []byte) {
	c.add(&cacheNode{offset: offset, payload: payload})
}

// putUnread holds the fact that the DATA object at offset holds a field
// the renderer does not read, at the cost of cacheEntryOverhead.
func (c *dataCache) putUnread(offset uint64) {
	c.add(&cacheNode{offset: offset, unread: true})
}

// add holds node, the most recently used, evicting the least recently
// used nodes until it fits.
func (c *dataCache) add(node *cacheNode) {
	cost := cacheCost(node.payload)
	if _, held := c.byOffset[node.offset]; held || cost > c.limit {
		return
	}
	for c.held+cost > c.limit {
		c.evict(c.tail)
	}
	c.byOffset[node.offset] = node
	c.pushFront(node)
	c.held += cost
	c.maxHeld = max(c.maxHeld, c.held)
}

// cacheCost is what holding payload counts against the limit.
func cacheCost(payload []byte) int64 {
	return int64(len(payload)) + cacheEntryOverhead
}

// evict drops node from the cache.
func (c *dataCache) evict(node *cacheNode) {
	c.unlink(node)
	delete(c.byOffset, node.offset)
	c.held -= cacheCost(node.payload)
}

// unlink takes node out of the list.
func (c *dataCache) unlink(node *cacheNode) {
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		c.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else {
		c.tail = node.prev
	}
	node.prev, node.next = nil, nil
}

// pushFront puts node at the head of the list.
func (c *dataCache) pushFront(node *cacheNode) {
	node.next = c.head
	if c.head != nil {
		c.head.prev = node
	}
	c.head = node
	if c.tail == nil {
		c.tail = node
	}
}
