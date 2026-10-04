package journal

// This file holds the DATA cache: what reading each DATA object gave,
// for the objects read last, by object offset. Entries share DATA
// objects (one hostname, one unit, one identifier serve thousands of
// entries), so an object read once serves every later entry that lists
// it: the decoded payload of a field the renderer reads, the fact that
// the object holds another field, whose value is never decoded whole, or
// the reason the object is damaged.

// defaultCacheBytes is the cache's bound when Options leaves it 0.
const defaultCacheBytes = 32 << 20

// cacheEntryOverhead is what the cache counts, on top of an answer's
// bytes, for holding it: the map entry and the list node. Counting it
// keeps a cache of many tiny answers within its bound too.
const cacheEntryOverhead = 96

// dataAnswer is what reading one DATA object gave the walk: the payload
// of a field the renderer reads; unread, for an object of any other
// field; or damage, the reason the object is damaged. Exactly one is set.
// The answer depends only on the object's bytes, so it holds for every
// entry that lists the object.
type dataAnswer struct {
	payload []byte
	unread  bool
	damage  string
}

// cost is what holding the answer counts against the cache's limit.
func (a dataAnswer) cost() int64 {
	return int64(len(a.payload)+len(a.damage)) + cacheEntryOverhead
}

// cacheNode is one answer the cache holds, in a list from the most
// recently used (head) to the least (tail).
type cacheNode struct {
	offset     uint64
	answer     dataAnswer
	prev, next *cacheNode
}

// dataCache holds what reading DATA objects gave, by object offset,
// least recently used out first, while the bytes it holds (each answer's
// cost) stay within limit. An answer that alone exceeds the limit is not
// held. The cache never changes a payload it hands out: the slices are
// shared with the entries that use them, which only read them.
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

// get returns the answer the cache holds for the DATA object at offset,
// if any, and marks it the most recently used.
func (c *dataCache) get(offset uint64) (dataAnswer, bool) {
	node, ok := c.byOffset[offset]
	if !ok {
		c.misses++
		return dataAnswer{}, false
	}
	c.hits++
	c.unlink(node)
	c.pushFront(node)
	return node.answer, true
}

// put holds answer as the DATA object at offset's, the most recently
// used, evicting the least recently used answers until it fits. An
// object already held, or an answer that costs more than the limit, is
// left as it is.
func (c *dataCache) put(offset uint64, answer dataAnswer) {
	cost := answer.cost()
	if _, held := c.byOffset[offset]; held || cost > c.limit {
		return
	}
	for c.held+cost > c.limit {
		c.evict(c.tail)
	}
	node := &cacheNode{offset: offset, answer: answer}
	c.byOffset[offset] = node
	c.pushFront(node)
	c.held += cost
	c.maxHeld = max(c.maxHeld, c.held)
}

// evict drops node from the cache.
func (c *dataCache) evict(node *cacheNode) {
	c.unlink(node)
	delete(c.byOffset, node.offset)
	c.held -= node.answer.cost()
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
