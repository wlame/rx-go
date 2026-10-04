package journal

import (
	"bytes"
	"testing"
)

// payloadOf returns a payload of size bytes, cost the cache's cost of
// holding one of them.
func payloadOf(size int) []byte { return bytes.Repeat([]byte{'x'}, size) }

func TestDataCache_GivesBackWhatItHolds(t *testing.T) {
	c := newDataCache(1 << 20)
	c.put(64, []byte("A=1"))
	c.put(128, []byte("B=2"))
	if got, _, ok := c.get(64); !ok || string(got) != "A=1" {
		t.Errorf("get(64) = %q, %v", got, ok)
	}
	if got, _, ok := c.get(128); !ok || string(got) != "B=2" {
		t.Errorf("get(128) = %q, %v", got, ok)
	}
	if _, _, ok := c.get(192); ok {
		t.Error("get(192) found a payload never put")
	}
	if c.hits != 2 || c.misses != 1 {
		t.Errorf("hits %d, misses %d, want 2 and 1", c.hits, c.misses)
	}
}

func TestDataCache_EvictsTheLeastRecentlyUsedFirst(t *testing.T) {
	const size = 1000
	c := newDataCache(3 * (size + cacheEntryOverhead))
	c.put(8, payloadOf(size))
	c.put(16, payloadOf(size))
	c.put(24, payloadOf(size))
	c.get(8) // 16 is now the least recently used
	c.put(32, payloadOf(size))
	for offset, want := range map[uint64]bool{8: true, 16: false, 24: true, 32: true} {
		if _, _, ok := c.get(offset); ok != want {
			t.Errorf("after the fourth put, get(%d) found = %v, want %v", offset, ok, want)
		}
	}
}

func TestDataCache_HoldsNoMoreThanItsBound(t *testing.T) {
	const limit = 10_000
	c := newDataCache(limit)
	size := 0
	for offset := uint64(8); offset <= 4000; offset += 8 {
		size = (size + 30) % 2910
		c.put(offset, payloadOf(size))
		if c.held > limit {
			t.Fatalf("after the put at %d the cache holds %d bytes, over its bound of %d", offset, c.held, limit)
		}
	}
	if c.maxHeld > limit || c.maxHeld == 0 {
		t.Errorf("maxHeld %d, want between 1 and %d", c.maxHeld, limit)
	}
}

func TestDataCache_DoesNotHoldAPayloadLargerThanItsBound(t *testing.T) {
	c := newDataCache(4096)
	c.put(8, []byte("SMALL=1"))
	c.put(16, payloadOf(4096))
	if _, _, ok := c.get(16); ok {
		t.Error("a payload larger than the bound was cached")
	}
	if _, _, ok := c.get(8); !ok {
		t.Error("caching nothing evicted the small payload")
	}
}

func TestDataCache_WithNoRoomHoldsNothing(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		c := newDataCache(limit)
		c.put(8, []byte("A=1"))
		if _, _, ok := c.get(8); ok || c.held != 0 {
			t.Errorf("limit %d: cached a payload, holding %d bytes", limit, c.held)
		}
	}
}

func TestOptions_CacheBytesZeroMeansTheDefault(t *testing.T) {
	if got := (Options{}).cacheBytes(); got != defaultCacheBytes || defaultCacheBytes != 32<<20 {
		t.Errorf("cacheBytes() = %d with defaultCacheBytes %d, want 32 MiB", got, defaultCacheBytes)
	}
	if got := (Options{CacheBytes: 1 << 20}).cacheBytes(); got != 1<<20 {
		t.Errorf("cacheBytes() = %d, want 1 MiB", got)
	}
}

func TestDataCache_HoldsAnUnreadFieldAtTheOverheadAlone(t *testing.T) {
	c := newDataCache(1 << 20)
	c.putUnread(64)
	payload, unread, ok := c.get(64)
	if !ok || !unread || payload != nil {
		t.Errorf("get(64) = %q, unread %v, cached %v; want an unread field", payload, unread, ok)
	}
	if c.held != cacheEntryOverhead {
		t.Errorf("an unread field costs %d bytes, want %d", c.held, cacheEntryOverhead)
	}
	c.put(128, []byte("MESSAGE=x"))
	if _, unread, _ := c.get(128); unread {
		t.Error("a payload is held as an unread field")
	}
}
