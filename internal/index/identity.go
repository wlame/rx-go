package index

import (
	"os"
	"time"
)

// SourceIdentity is what a cache records about the file it was built
// from, so that a later run can tell whether the file on disk is still
// that file. The line index and the trace cache both stamp one and both
// check it through MatchesFile, so the two caches agree on when a file
// has changed.
//
// Size and mtime alone cannot tell a rewritten file from an untouched
// one: a copy made with `cp -p` restores the mtime, and an edit that
// swaps one byte for another keeps the byte count. The inode and the
// device change when a file is replaced by rename or another file takes
// its path, the ctime moves on every write and cannot be set through
// utime, and the fingerprint (a digest of the size plus the first and
// last 64 KiB, see SourceFingerprint) catches a rewrite on a filesystem
// that reports neither faithfully.
//
// Times are compared as nanoseconds since the Unix epoch, which name
// one instant whatever the local time zone. The text fields ModifiedAt
// and ChangedAt record the same times as local wall-clock text for a
// person reading the cache file; they are written and never compared,
// since the same instant reads differently under another TZ, and two
// instants an hour apart read the same in the hour a daylight-saving
// change repeats.
//
// The pointer fields are nil when the value is not known: the platform
// reported no inode, device or ctime, or the fingerprint could not be
// read. MatchesFile does not compare a nil field.
type SourceIdentity struct {
	// SizeBytes is the file size in bytes.
	SizeBytes int64
	// ModifiedAt is the mtime in the layout FormatMtime produces, in
	// local time. Not compared.
	ModifiedAt string
	// ModifiedNs is the mtime in nanoseconds since the Unix epoch.
	ModifiedNs int64
	// Inode is the inode number, or nil.
	Inode *uint64
	// Device is the number of the device (filesystem) that holds the
	// inode, or nil. An inode number is unique only on its device.
	Device *uint64
	// ChangedAt is the inode-change time (ctime) in the FormatMtime
	// layout, in local time, or nil. Not compared.
	ChangedAt *string
	// ChangedNs is the ctime in nanoseconds since the Unix epoch, or nil.
	ChangedNs *int64
	// Fingerprint is SourceFingerprint of the file, or nil.
	Fingerprint *string
}

// statIdentity is what a platform's stat result says about a file
// beyond its size and mtime (see sourceIdentity).
type statIdentity struct {
	inode   uint64
	device  uint64
	changed time.Time
}

// InodeAndDevice returns the inode and device numbers info records, the
// pair that names one file on one machine, as every identity rx records
// reads them (the device widened the same way on each platform). ok is
// false on a platform whose stat gives neither; both numbers are then 0.
func InodeAndDevice(info os.FileInfo) (inode, device uint64, ok bool) {
	st, ok := sourceIdentity(info)
	return st.inode, st.device, ok
}

// IdentityFromInfo describes the file at path as the stat result info
// saw it. The fingerprint is read from path now, so a caller that wants
// the identity of a file at one moment stats it once and passes that
// result here, rather than stating again.
//
// If the file grows between the stat and the fingerprint read, the
// fingerprint covers the larger file while SizeBytes records the
// smaller one. MatchesFile then never accepts the identity, which costs
// a cache rebuild and never a wrong answer.
//
// A fingerprint that cannot be read is left nil rather than reported as
// an error; MatchesFile then falls back to the other fields.
func IdentityFromInfo(path string, info os.FileInfo) SourceIdentity {
	fp, err := SourceFingerprint(path)
	return identityOf(info, fp, err)
}

// IdentityFromOpenFile is IdentityFromInfo for a file already open,
// such as one a pin opened: the fingerprint is read from f, not from a
// path that may lead elsewhere by now. f's read offset is left where it
// was.
func IdentityFromOpenFile(f *os.File, info os.FileInfo) SourceIdentity {
	fp, err := fingerprintOpenFile(f)
	return identityOf(info, fp, err)
}

// identityOf assembles an identity from a stat result and a fingerprint
// read, leaving the fingerprint nil when the read failed (fpErr).
func identityOf(info os.FileInfo, fp string, fpErr error) SourceIdentity {
	id := SourceIdentity{
		SizeBytes:  info.Size(),
		ModifiedAt: formatMtime(info.ModTime()),
		ModifiedNs: info.ModTime().UnixNano(),
	}
	if st, ok := sourceIdentity(info); ok {
		changedAt := formatMtime(st.changed)
		changedNs := st.changed.UnixNano()
		id.Inode = &st.inode
		id.Device = &st.device
		id.ChangedAt = &changedAt
		id.ChangedNs = &changedNs
	}
	if fpErr == nil {
		id.Fingerprint = &fp
	}
	return id
}

// Equal reports whether id and other record the same file in the same
// state: every compared field equal, and a field missing from one
// missing from the other too. Two stats of one unchanged file give
// equal identities; a write, a replacement or a growth between them
// does not. The text times are left out, as MatchesFile leaves them
// out.
func (id SourceIdentity) Equal(other SourceIdentity) bool {
	return id.SizeBytes == other.SizeBytes &&
		id.ModifiedNs == other.ModifiedNs &&
		equalPointees(id.Inode, other.Inode) &&
		equalPointees(id.Device, other.Device) &&
		equalPointees(id.ChangedNs, other.ChangedNs) &&
		equalPointees(id.Fingerprint, other.Fingerprint)
}

// equalPointees reports whether a and b are both nil or point at equal
// values.
//
// Go note: [T comparable] makes this one function for every type whose
// values == can compare (uint64, int64 and string here).
func equalPointees[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// MatchesFile reports whether the file at path is still the file id
// describes. A file that cannot be stated does not match.
//
// The cheap checks run first (size, mtime, inode, device and ctime come
// from one stat call) so the two 64 KiB fingerprint reads happen only
// for a file that passed them.
func (id SourceIdentity) MatchesFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Size() != id.SizeBytes {
		return false
	}
	if info.ModTime().UnixNano() != id.ModifiedNs {
		return false
	}
	if !id.matchesStat(info) {
		return false
	}
	return id.matchesFingerprint(path)
}

// matchesStat compares the recorded inode, device and ctime with the
// ones in info.
//
// A nil field means the record predates the check or the filesystem did
// not report one, so an absent value is not treated as a mismatch. On a
// platform where the stat result carries none of them, there is nothing
// to compare and the check passes.
func (id SourceIdentity) matchesStat(info os.FileInfo) bool {
	st, ok := sourceIdentity(info)
	if !ok {
		return true
	}
	if !id.sameInodeAndDevice(st) {
		return false
	}
	return id.ChangedNs == nil || *id.ChangedNs == st.changed.UnixNano()
}

// sameInodeAndDevice reports whether st is the file the recorded inode
// and device name. An inode number is unique only on its device, so
// two files on two filesystems can share one; the device tells them
// apart. A nil field is not compared.
func (id SourceIdentity) sameInodeAndDevice(st statIdentity) bool {
	if id.Inode != nil && *id.Inode != st.inode {
		return false
	}
	return id.Device == nil || *id.Device == st.device
}

// matchesFingerprint re-reads the two 64 KiB windows the fingerprint
// covers and compares the digest.
//
// A nil fingerprint is not a mismatch, so a record written before the
// field existed still validates on the other fields. A read failure is
// a mismatch: if we cannot confirm the file is the one that was
// recorded, the record does not get used.
func (id SourceIdentity) matchesFingerprint(path string) bool {
	if id.Fingerprint == nil {
		return true
	}
	current, err := SourceFingerprint(path)
	if err != nil {
		return false
	}
	return current == *id.Fingerprint
}
