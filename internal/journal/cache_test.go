package journal

import (
	"bytes"
	"testing"
)

// payloadOf returns the answer of a field the renderer reads whose
// payload is size bytes.
func payloadOf(size int) dataAnswer {
	return dataAnswer{payload: bytes.Repeat([]byte{'x'}, size)}
}

// answerOf returns the answer of a field the renderer reads whose payload
// is s.
func answerOf(s string) dataAnswer { return dataAnswer{payload: []byte(s)} }

func TestDataCache_GivesBackWhatItHolds(t *testing.T) {
	c := newDataCache(1 << 20)
	c.put(64, answerOf("A=1"))
	c.put(128, answerOf("B=2"))
	if got, ok := c.get(64); !ok || string(got.payload) != "A=1" {
		t.Errorf("get(64) = %q, %v", got.payload, ok)
	}
	if got, ok := c.get(128); !ok || string(got.payload) != "B=2" {
		t.Errorf("get(128) = %q, %v", got.payload, ok)
	}
	if _, ok := c.get(192); ok {
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
		if _, ok := c.get(offset); ok != want {
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
	c.put(8, answerOf("SMALL=1"))
	c.put(16, payloadOf(4096))
	if _, ok := c.get(16); ok {
		t.Error("a payload larger than the bound was cached")
	}
	if _, ok := c.get(8); !ok {
		t.Error("caching nothing evicted the small payload")
	}
}

func TestDataCache_WithNoRoomHoldsNothing(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		c := newDataCache(limit)
		c.put(8, answerOf("A=1"))
		if _, ok := c.get(8); ok || c.held != 0 {
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

func TestDataCache_HoldsUnreadFieldsAndDamageAtTheirCost(t *testing.T) {
	c := newDataCache(1 << 20)
	c.put(64, dataAnswer{unread: true})
	if got, ok := c.get(64); !ok || !got.unread || got.payload != nil {
		t.Errorf("get(64) = %+v, %v; want an unread field", got, ok)
	}
	if c.held != cacheEntryOverhead {
		t.Errorf("an unread field costs %d bytes, want %d", c.held, cacheEntryOverhead)
	}
	reason := "the DATA object at offset 128 does not decompress (zstd)"
	c.put(128, dataAnswer{damage: reason})
	if got, ok := c.get(128); !ok || got.damage != reason {
		t.Errorf("get(128) = %+v, %v; want the damage", got, ok)
	}
	if want := int64(2*cacheEntryOverhead + len(reason)); c.held != want {
		t.Errorf("the cache holds %d bytes, want %d", c.held, want)
	}
}
