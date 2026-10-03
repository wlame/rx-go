package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// fingerprintWindow is how many bytes are read from each end of the file
// to build the fingerprint. 64 KiB is small enough that validating an
// index costs two short reads even on a multi-gigabyte log, and large
// enough to cover the header and the tail where files are edited most.
const fingerprintWindow = 64 * 1024

// SourceFingerprint summarizes a file cheaply enough to check on every
// index load.
//
// Size and mtime cannot tell a rewritten file from an untouched one: a
// copy made with `cp -p` restores the mtime, and an edit that swaps one
// byte for another keeps the byte count. Either leaves a stale index
// looking valid, and a stale index answers with the wrong line number.
//
// The digest covers the size, the first 64 KiB, and the last 64 KiB. It
// is deliberately not a whole-file hash: reading 80 GB to decide whether
// an index is usable would cost more than rebuilding it. So this closes
// the ordinary cases — a rewritten header, a rotated log, a truncated
// and refilled file — and does not claim to catch an edit confined to
// the middle of a large file that also preserves the size and the mtime.
// The inode and ctime recorded beside it cover that case on any
// filesystem that reports them faithfully.
//
// The layout is part of the on-disk format and must match rx-python's
// source_fingerprint byte for byte.
func SourceFingerprint(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from a validated search root
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return fingerprintOpenFile(f)
}

// fingerprintOpenFile is SourceFingerprint of a file already open: the
// one a pin opened, so the digest is of that file whatever its path
// leads to by now.
//
// It reads by position (io.NewSectionReader uses ReadAt), so the
// file's read offset is left where it was and a caller can fingerprint
// a file and then read it from the start. io.CopyN fails on a short
// read, so a file that shrinks during the read gets no fingerprint.
func fingerprintOpenFile(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()

	h := sha256.New()
	// The size goes in first so that a prefix which happens to repeat
	// cannot produce the same digest at two different lengths.
	_, _ = fmt.Fprintf(h, "%d\n", size)

	head := min(size, fingerprintWindow)
	if head > 0 {
		if _, err := io.CopyN(h, io.NewSectionReader(f, 0, head), head); err != nil {
			return "", fmt.Errorf("fingerprint head of %s: %w", f.Name(), err)
		}
	}
	// Only read a tail window when the file is long enough for it to
	// cover bytes the head window did not.
	if size > fingerprintWindow {
		tail := io.NewSectionReader(f, size-fingerprintWindow, fingerprintWindow)
		if _, err := io.CopyN(h, tail, fingerprintWindow); err != nil {
			return "", fmt.Errorf("fingerprint tail of %s: %w", f.Name(), err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
