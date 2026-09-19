package index

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/wlame/rx-go/internal/config"
)

// MaxCacheFileNameBytes is the longest file name rx gives a cache file:
// 255 bytes is the limit of one path component on the filesystems rx
// runs on (ext4, xfs, btrfs, APFS). Both the line index and the trace
// cache cut the source's base name inside their file names so that the
// whole name fits; the hash in the name keeps two long names apart.
//
// A filesystem with a lower limit (eCryptfs allows about 143 bytes)
// still refuses the longest names; a lookup there reads the file
// without an index, and the failed write says why.
const MaxCacheFileNameBytes = 255

// maxCacheNamePartBytes is the room the base name has in a line index
// file name "<name>_<hash16>.json": the limit minus the underscore, the
// 16 hex digits of the hash and the ".json" suffix. A base name of this
// length or less is kept whole, so every index stored before the cut
// existed keeps its file name.
const maxCacheNamePartBytes = MaxCacheFileNameBytes - len("_") - 16 - len(".json")

// TrimCacheNamePart returns name cut to at most maxBytes bytes. When
// name is UTF-8 the cut never falls inside a character; bytes that are
// not UTF-8 are cut wherever the limit falls.
func TrimCacheNamePart(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	cut := maxBytes
	// utf8.RuneStart is false for the continuation bytes of a
	// multibyte character, so stepping back over them lands on the
	// first byte of the character the limit falls inside, which is then
	// left out whole. The loop steps back at most utf8.UTFMax-1 bytes
	// for UTF-8; for other bytes it stops at the first one that is not
	// a continuation byte, or at 0.
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut]
}

// IsNoCacheEntry reports whether err, from reading a cache file, means
// that no file can be there: the file does not exist, a component of
// its directory is a regular file (RX_CACHE_DIR set to a file), or the
// path is too long to name a file. Such a read is an ordinary miss,
// never an entry that exists and cannot be read.
func IsNoCacheEntry(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ENAMETOOLONG)
}

// CheckStorable reports whether a line index can be stored in the index
// cache directory right now, and why not when it cannot: the directory
// cannot be created (RX_CACHE_DIR names a regular file, a parent is
// read-only) or a file cannot be created in it (it is read-only, or the
// disk refuses). A caller asks before it spends a build on an index that
// Save would then fail to store.
//
// The check creates the directory when it is missing, and creates and
// removes one empty temporary file named ".tmp-probe-*": the same kind
// of name Save writes through, which nothing ever reads as an index.
func CheckStorable() error {
	dir := config.GetIndexCacheDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create the index cache directory %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".tmp-probe-*")
	if err != nil {
		return fmt.Errorf("write in the index cache directory %s: %w", dir, err)
	}
	// The probe has served its purpose once it exists; a failure to
	// close or remove an empty file is not a reason to refuse a build.
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}
