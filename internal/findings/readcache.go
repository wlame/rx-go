package findings

import (
	"sync"
	"unsafe"

	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/sigscan"
)

// readCache keeps, for the files Read answered most recently, the cache
// entries and masked ranges it decoded, so a later Read of the same
// file decodes no entry file again: the HTTP findings route reads one
// file's findings on every page, filter, F8 and timeline redraw.
//
// It is never stale. A file's record holds the file's identity
// (index.SourceIdentity, fingerprint included) and each entry's key
// and stored count, and Read uses a record only for the same identity,
// the same key and count meta.json names now: an entry is a pure
// function of the file's text and its key (the same answer on every
// path), so a match holds what the entry file holds. meta.json and the
// file's identity are read on every Read, as without the cache.
//
// It is bounded: at most maxFiles files and maxBytes bytes of decoded
// findings (an estimate: the Finding structs and their excerpts, the
// series and the ranges), the least recently read file out first. A
// file whose answer alone is over maxBytes is not kept.
type readCache struct {
	mu       sync.Mutex
	maxFiles int
	maxBytes int64
	bytes    int64
	// clock orders the reads: a file's used is the clock of its last
	// read, and the smallest is the least recently read.
	clock uint64
	files map[string]*decodedFile
}

// decodedFile is what the read cache holds for one file directory.
type decodedFile struct {
	identity index.SourceIdentity
	ranges   *decodedRanges
	entries  map[string]decodedEntry
	bytes    int64
	used     uint64
}

// decodedRanges is a ranges file as Read decoded it: its key, the
// number of its ranges, and the ranges and scanned end it holds.
type decodedRanges struct {
	key   string
	count int
	text  scannedText
}

// decodedEntry is an entry file as Read decoded it: its key, how many
// findings it stores, its findings and its series, and the last line of
// its findings (the check against the scanned end).
type decodedEntry struct {
	key      string
	findings []sigscan.Finding
	series   *sigscan.Series
	lastLine int64
}

// findingBytes is the decoded size of one finding without its excerpt:
// the struct and its three optional numbers.
const findingBytes = int64(unsafe.Sizeof(sigscan.Finding{})) + 24

// newReadCache returns a read cache of at most files files and bytes
// bytes, or nil (no cache) when either is 0 or less.
func newReadCache(files int, bytes int64) *readCache {
	if files <= 0 || bytes <= 0 {
		return nil
	}
	return &readCache{maxFiles: files, maxBytes: bytes, files: map[string]*decodedFile{}}
}

// lookup returns a copy of what the cache holds for dir in the state
// identity, and marks it read; nil when it holds nothing for that state.
// The copy's findings are shared and never changed.
func (c *readCache) lookup(dir string, identity index.SourceIdentity) *decodedFile {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.files[dir]
	if f == nil || !f.identity.Equal(identity) {
		return nil
	}
	c.clock++
	f.used = c.clock
	snapshot := *f
	snapshot.entries = make(map[string]decodedEntry, len(f.entries))
	for id, e := range f.entries {
		snapshot.entries[id] = e
	}
	return &snapshot
}

// store adds what a Read decoded from dir's files in the state identity,
// which the Read checked: the ranges (nil when they came from the cache)
// and the entries. A record of another state is replaced. A record
// over maxBytes on its own is dropped, and nothing else; otherwise
// files are evicted, the least recently read first, until the bounds
// hold.
func (c *readCache) store(dir string, identity index.SourceIdentity, ranges *decodedRanges, entries map[string]decodedEntry) {
	if c == nil || (ranges == nil && len(entries) == 0) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.files[dir]
	if f == nil || !f.identity.Equal(identity) {
		if f != nil {
			c.bytes -= f.bytes
		}
		f = &decodedFile{identity: identity, entries: map[string]decodedEntry{}}
		c.files[dir] = f
	}
	if ranges != nil {
		if f.ranges != nil {
			f.bytes -= rangesBytes(f.ranges)
			c.bytes -= rangesBytes(f.ranges)
		}
		f.ranges = ranges
		f.bytes += rangesBytes(ranges)
		c.bytes += rangesBytes(ranges)
	}
	for id, e := range entries {
		if old, ok := f.entries[id]; ok {
			f.bytes -= entryBytes(old)
			c.bytes -= entryBytes(old)
		}
		f.entries[id] = e
		f.bytes += entryBytes(e)
		c.bytes += entryBytes(e)
	}
	if f.bytes > c.maxBytes {
		// This answer alone is over the cap, so it is not kept; the
		// other files stay, since evicting them would not make room.
		c.bytes -= f.bytes
		delete(c.files, dir)
		return
	}
	c.clock++
	f.used = c.clock
	c.evictLocked()
}

// evictLocked drops the least recently read files until at most
// maxFiles files and maxBytes bytes are held. The caller holds c.mu.
func (c *readCache) evictLocked() {
	for len(c.files) > c.maxFiles || c.bytes > c.maxBytes {
		oldest := ""
		for dir, f := range c.files {
			if oldest == "" || f.used < c.files[oldest].used {
				oldest = dir
			}
		}
		c.bytes -= c.files[oldest].bytes
		delete(c.files, oldest)
	}
}

// entryBytes is the decoded size of an entry.
func entryBytes(e decodedEntry) int64 {
	n := int64(len(e.key)) + int64(len(e.findings))*findingBytes
	for _, f := range e.findings {
		n += int64(len(f.Excerpt))
	}
	if e.series != nil {
		n += int64(len(e.series.Counts)) * 8
	}
	return n
}

// rangesBytes is the decoded size of a ranges file.
func rangesBytes(r *decodedRanges) int64 {
	return int64(len(r.key)) + int64(len(r.text.ranges))*16
}

// decodedEntryOf keeps an entry file's findings for the read cache.
func decodedEntryOf(entry *entryFile, result *sigscan.SignatureResult) decodedEntry {
	e := decodedEntry{key: entry.Key, findings: result.Findings, series: result.Series}
	for _, f := range result.Findings {
		e.lastLine = max(e.lastLine, f.EndLine)
	}
	return e
}
